package tart

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed endpoint/bridge.py
var endpointScript []byte

func (b *Client) endpointLabel(uid string) string { return "dev.pulse-tart.engine." + uid }
func service(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/launchctl", args...)
	var output boundedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	return output.Bytes(), err
}
func serviceAbsent(err error) bool {
	var exited *exec.ExitError
	return errors.As(err, &exited) && exited.ExitCode() == 113
}
func endpointFailure(dir, stage string, output []byte, err error) error {
	if len(output) > 8192 {
		output = output[len(output)-8192:]
	}
	detail := fmt.Sprintf("%s: %v\n%s", stage, err, strings.TrimSpace(string(output)))
	_ = os.WriteFile(filepath.Join(dir, "last-error.log"), []byte(detail+"\n"), 0600)
	return fmt.Errorf("Docker endpoint %s: %w", detail, Err)
}
func (b *Client) Endpoint(ctx context.Context, name, uid string, port int) (bool, error) {
	if e := b.mu.LockContext(ctx); e != nil {
		return false, e
	}
	defer b.mu.Unlock()
	v, e := b.get(ctx, name)
	if e != nil || v.UID != uid || v.Stopped() {
		return false, fmt.Errorf("Docker endpoint engine identity/state unavailable: %w", Err)
	}
	data, e := b.command(ctx, "ip", name, "--wait", "5")
	if e != nil {
		return false, fmt.Errorf("Docker endpoint engine IP unavailable: %w", e)
	}
	ip := strings.TrimSpace(string(data))
	if net.ParseIP(ip) == nil {
		return false, Err
	}
	return b.ensureEndpoint(ctx, uid, net.JoinHostPort(ip, "2222"), port)
}

// The caller has verified VM ownership. This supervisor also has a VM-free Mac
// preflight so launchd failures are caught before expensive image preparation.
func (b *Client) ensureEndpoint(ctx context.Context, uid, target string, port int) (bool, error) {
	dir := filepath.Join(b.dir, "endpoints", uid)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return false, e
	}
	ready := filepath.Join(dir, "ready.json")
	var state struct {
		Port   int
		Target string
	}
	domain := "user/" + strconv.Itoa(os.Getuid())
	label := b.endpointLabel(uid)
	output, e := b.service(ctx, "print", domain+"/"+label)
	if e != nil && !serviceAbsent(e) {
		return false, endpointFailure(dir, "inspect launchd service", output, e)
	}
	if e == nil {
		bytes, err := os.ReadFile(ready)
		if err == nil && json.Unmarshal(bytes, &state) == nil && state.Port == port && state.Target == target {
			return true, nil
		}
		// Restart the owned helper when DHCP changes its target. Missing readiness
		// alone can mean launchd is still starting it; do not restart that case.
		if err != nil || state.Target == "" || state.Target == target {
			return false, nil
		}
		if output, e = b.service(ctx, "bootout", domain+"/"+label); e != nil {
			return false, endpointFailure(dir, "stop old target", output, e)
		}
	}
	_ = os.Remove(ready)
	python := b.cfg.PythonBinary
	if python == "" {
		python = "/usr/bin/python3"
	}
	if !filepath.IsAbs(python) {
		return false, Err
	}
	script := filepath.Join(dir, "bridge.py")
	plist := filepath.Join(dir, "service.plist")
	if e = os.WriteFile(script, endpointScript, 0600); e != nil {
		return false, e
	}
	args := []string{python, script, "--target", target, "--ready", ready, "--port", strconv.Itoa(port)}
	var arr strings.Builder
	for _, a := range args {
		arr.WriteString("<string>" + xmlEscape(a) + "</string>")
	}
	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>Label</key><string>%s</string><key>ProgramArguments</key><array>%s</array><key>LimitLoadToSessionType</key><string>Background</string><key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>ThrottleInterval</key><integer>2</integer><key>StandardOutPath</key><string>%s</string><key>StandardErrorPath</key><string>%s</string></dict></plist>`, label, arr.String(), xmlEscape(filepath.Join(dir, "service.log")), xmlEscape(filepath.Join(dir, "service.log")))
	if e = os.WriteFile(plist, []byte(content), 0600); e != nil {
		return false, e
	}
	if output, e = b.service(ctx, "bootstrap", domain, plist); e != nil {
		return false, endpointFailure(dir, "bootstrap launchd service", output, e)
	}
	return false, nil
}
func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;").Replace(s)
}
func (b *Client) RemoveEndpoint(ctx context.Context, name, uid string) error {
	domain := "user/" + strconv.Itoa(os.Getuid())
	label := b.endpointLabel(uid)
	dir := filepath.Join(b.dir, "endpoints", uid)
	output, err := b.service(ctx, "print", domain+"/"+label)
	if err == nil {
		if output, err = b.service(ctx, "bootout", domain+"/"+label); err != nil {
			return endpointFailure(dir, "remove launchd service", output, err)
		}
	} else if !serviceAbsent(err) {
		return endpointFailure(dir, "inspect before removal", output, err)
	}
	return os.RemoveAll(filepath.Join(b.dir, "endpoints", uid))
}

// EndpointDiagnostics contains only endpoint configuration and bounded helper
// logs. Never include allocation journals, SSH keys or inherited environments.
func (b *Client) EndpointDiagnostics(ctx context.Context) (map[string]map[string]string, error) {
	result := map[string]map[string]string{}
	entries, err := os.ReadDir(filepath.Join(b.dir, "endpoints"))
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(b.dir, "endpoints", entry.Name())
		files := map[string]string{}
		for _, name := range []string{"service.plist", "ready.json", "service.log", "last-error.log"} {
			f, err := os.Open(filepath.Join(dir, name))
			if err != nil {
				continue
			}
			if info, err := f.Stat(); err == nil && info.Size() > 16384 {
				_, _ = f.Seek(-16384, io.SeekEnd)
			}
			content, _ := io.ReadAll(io.LimitReader(f, 16384))
			f.Close()
			files[name] = string(content)
		}
		output, err := b.service(ctx, "print", "user/"+strconv.Itoa(os.Getuid())+"/"+b.endpointLabel(entry.Name()))
		if err != nil {
			files["launchctl-status"] = fmt.Sprint(err)
		}
		// launchctl print can include inherited environment variables: retain only
		// process/lifecycle facts, never the full environment dump.
		for _, line := range strings.Split(string(output), "\n") {
			if !strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "\t\t") {
				continue
			}
			line = strings.TrimSpace(line)
			for _, prefix := range []string{"state = ", "pid = ", "runs = ", "last exit code = ", "last terminating signal = "} {
				if strings.HasPrefix(line, prefix) {
					files["launchctl-status"] += "\n" + line
					break
				}
			}
		}
		result[entry.Name()] = files
	}
	return result, nil
}
