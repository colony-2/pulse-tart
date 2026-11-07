package tart

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

//go:embed endpoint/bridge.py
var endpointScript []byte

func (b *Client) endpointLabel(uid string) string { return "dev.pulse-tart.engine." + uid }
func service(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "/bin/launchctl", args...)
	return cmd.Run()
}
func (b *Client) Endpoint(ctx context.Context, name, uid string, port int) (bool, error) {
	if e := b.mu.LockContext(ctx); e != nil {
		return false, e
	}
	defer b.mu.Unlock()
	v, e := b.get(ctx, name)
	if e != nil || v.UID != uid || v.Stopped() {
		return false, Err
	}
	data, e := b.command(ctx, "ip", name, "--wait", "5")
	if e != nil {
		return false, e
	}
	ip := strings.TrimSpace(string(data))
	if net.ParseIP(ip) == nil {
		return false, Err
	}
	dir := filepath.Join(b.dir, "endpoints", uid)
	if e = os.MkdirAll(dir, 0700); e != nil {
		return false, e
	}
	ready := filepath.Join(dir, "ready.json")
	var state struct {
		Port   int
		Target string
	}
	domain := "user/" + strconv.Itoa(os.Getuid())
	label := b.endpointLabel(uid)
	active := service(ctx, "print", domain+"/"+label) == nil
	if active {
		bytes, err := os.ReadFile(ready)
		if err == nil && json.Unmarshal(bytes, &state) == nil && state.Port == port && state.Target == net.JoinHostPort(ip, "2222") {
			return true, nil
		}
		// Restart the owned helper when DHCP changes its target. Missing readiness
		// alone can mean launchd is still starting it; do not restart that case.
		if err != nil || state.Target == "" || state.Target == net.JoinHostPort(ip, "2222") {
			return false, nil
		}
		if service(ctx, "bootout", domain+"/"+label) != nil {
			return false, Err
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
	args := []string{python, script, "--target", net.JoinHostPort(ip, "2222"), "--ready", ready, "--port", strconv.Itoa(port)}
	var arr strings.Builder
	for _, a := range args {
		arr.WriteString("<string>" + xmlEscape(a) + "</string>")
	}
	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>Label</key><string>%s</string><key>ProgramArguments</key><array>%s</array><key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>ThrottleInterval</key><integer>2</integer><key>StandardOutPath</key><string>%s</string><key>StandardErrorPath</key><string>%s</string></dict></plist>`, label, arr.String(), xmlEscape(filepath.Join(dir, "service.log")), xmlEscape(filepath.Join(dir, "service.log")))
	if e = os.WriteFile(plist, []byte(content), 0600); e != nil {
		return false, e
	}
	if e = service(ctx, "bootstrap", domain, plist); e != nil {
		return false, Err
	}
	return false, nil
}
func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;").Replace(s)
}
func (b *Client) RemoveEndpoint(ctx context.Context, name, uid string) error {
	domain := "user/" + strconv.Itoa(os.Getuid())
	label := b.endpointLabel(uid)
	err := service(ctx, "print", domain+"/"+label)
	if err == nil {
		if service(ctx, "bootout", domain+"/"+label) != nil {
			return Err
		}
	} else {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var exited *exec.ExitError
		// launchctl's missing-service status is the only confirmed absence.
		if !errors.As(err, &exited) || exited.ExitCode() != 113 {
			return Err
		}
	}
	return os.RemoveAll(filepath.Join(b.dir, "endpoints", uid))
}
