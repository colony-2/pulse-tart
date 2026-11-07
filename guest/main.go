// pulse-tart-guest performs durable one-shot delivery. It never prints a payload.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const stateDir = "/var/db/pulse-tart"
const workerUID = 55050

var workerGID uint32 = 20
var workerHome = "/Users/pulseworker"

func init() {
	if runtime.GOOS == "linux" {
		workerGID = 55050
		workerHome = "/home/pulseworker"
	}
}

const profile = "/opt/pulse-tart"

type Process struct {
	Command    []string          `json:"command"`
	Args       []string          `json:"args"`
	Env        map[string]string `json:"env"`
	WorkingDir string            `json:"working_dir"`
	Stdin      string            `json:"stdin,omitempty"`
	StdinBytes []byte            `json:"stdin_bytes,omitempty"`
}
type Envelope struct {
	Version  int     `json:"version"`
	LaunchID string  `json:"launch_id"`
	Process  Process `json:"process"`
}

var invalid = errors.New("invalid launch envelope")

func validate(e Envelope) error {
	p := e.Process
	if e.Version != 1 || !regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`).MatchString(e.LaunchID) || len(p.Command) == 0 || p.Command[0] == "" || len(p.Stdin)+len(p.StdinBytes) > 1<<20 || (p.Stdin != "" && p.StdinBytes != nil) {
		return invalid
	}
	for _, s := range append(append([]string{p.WorkingDir}, p.Command...), p.Args...) {
		if strings.ContainsRune(s, 0) {
			return invalid
		}
	}
	if p.WorkingDir != "" && !filepath.IsAbs(p.WorkingDir) {
		return invalid
	}
	for k, v := range p.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
			return invalid
		}
	}
	return nil
}
func decode(r io.Reader) (Envelope, error) {
	var e Envelope
	b, err := io.ReadAll(io.LimitReader(r, 16<<20+1))
	if err != nil || len(b) > 16<<20 {
		return e, invalid
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&e) != nil || validate(e) != nil {
		return e, invalid
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return e, invalid
	}
	return e, nil
}
func writeAtomic(dir, name string, b []byte) error {
	f, e := os.CreateTemp(dir, ".pending-")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e == nil {
		e = os.Rename(tmp, filepath.Join(dir, name))
	}
	if e == nil {
		d, er := os.Open(dir)
		if er != nil {
			return er
		}
		e = d.Sync()
		d.Close()
	}
	return e
}
func ingest(dir string, r io.Reader) error {
	e, err := decode(r)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "ingest.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX) != nil {
		return invalid
	}
	b, _ := json.Marshal(e)
	hash := sha256.Sum256(b)
	fingerprint := []byte(hex.EncodeToString(hash[:]))
	if old, err := os.ReadFile(filepath.Join(dir, "fingerprint")); err == nil {
		if !bytes.Equal(old, fingerprint) {
			return invalid
		}
		return nil
	} else if !os.IsNotExist(err) {
		return invalid
	}
	// Queue first, fingerprint second. Recovery never replaces an existing queue.
	if old, err := os.ReadFile(filepath.Join(dir, "launch.json")); err == nil && !bytes.Equal(old, b) {
		return invalid
	}
	if err = writeAtomic(dir, "launch.json", b); err != nil {
		return err
	}
	return writeAtomic(dir, "fingerprint", fingerprint)
}
func environment(p Process) []string {
	env := map[string]string{"PATH": profile + "/bin:/nix/var/nix/profiles/default/bin:/usr/bin:/bin:/usr/sbin:/sbin", "HOME": workerHome, "USER": "pulseworker", "LOGNAME": "pulseworker", "TMPDIR": "/tmp", "SSL_CERT_FILE": profile + "/etc/ssl/certs/ca-bundle.crt", "NIX_SSL_CERT_FILE": profile + "/etc/ssl/certs/ca-bundle.crt", "C2J_TOOL_CACHE_DIR": workerHome + "/.cache/c2j/tools"}
	for k, v := range p.Env {
		env[k] = v
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(env))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}
func resolve(command, dir string, env []string) (string, error) {
	if strings.Contains(command, "/") {
		if !filepath.IsAbs(command) {
			return filepath.Join(dir, command), nil
		}
		return command, nil
	}
	path := ""
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			path = strings.TrimPrefix(e, "PATH=")
		}
	}
	for _, d := range filepath.SplitList(path) {
		if !filepath.IsAbs(d) {
			d = filepath.Join(dir, d)
		}
		p := filepath.Join(d, command)
		s, e := os.Stat(p)
		if e == nil && s.Mode().IsRegular() && s.Mode().Perm()&0111 != 0 {
			return p, nil
		}
	}
	return "", errors.New("worker executable unavailable")
}
func execute(e Envelope, dir string, uid, gid uint32) (int, error) {
	// Even failed executable lookup consumes this one-shot launch.
	if err := writeAtomic(dir, "intent", []byte(e.LaunchID)); err != nil {
		return -1, err
	}
	p := e.Process
	argv := append(append([]string{}, p.Command...), p.Args...)
	env := environment(p)
	cwd := p.WorkingDir
	if cwd == "" {
		cwd = workerHome
	}
	path, err := resolve(argv[0], cwd, env)
	if err != nil {
		return -1, err
	}
	cmd := exec.Command(path, argv[1:]...)
	cmd.Args = argv
	cmd.Env = env
	cmd.Dir = cwd
	cmd.Stdin = strings.NewReader(p.Stdin)
	if p.StdinBytes != nil {
		cmd.Stdin = bytes.NewReader(p.StdinBytes)
	}
	log, err := os.OpenFile(filepath.Join(dir, "worker.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return -1, err
	}
	defer log.Close()
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if os.Geteuid() == 0 {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uid, Gid: gid}
		if runtime.GOOS == "linux" {
			if u, e := user.LookupId(strconv.FormatUint(uint64(uid), 10)); e == nil {
				if groups, e := u.GroupIds(); e == nil {
					for _, g := range groups {
						if n, e := strconv.ParseUint(g, 10, 32); e == nil {
							cmd.SysProcAttr.Credential.Groups = append(cmd.SysProcAttr.Credential.Groups, uint32(n))
						}
					}
				}
			}
		}
	}
	if err = cmd.Start(); err != nil {
		return -1, errors.New("worker start failed")
	}
	if err = writeAtomic(dir, "pid", []byte(strconv.Itoa(cmd.Process.Pid))); err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		return -1, err
	}
	err = cmd.Wait()
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	code := 0
	if err != nil {
		code = 1
		if x, ok := err.(*exec.ExitError); ok {
			code = x.ExitCode()
		}
	}
	_ = writeAtomic(dir, "complete", []byte(strconv.Itoa(code)))
	_ = os.Remove(filepath.Join(dir, "launch.json"))
	return code, nil
}
func runningWorker() bool {
	return exec.Command("/usr/bin/pgrep", "-u", strconv.Itoa(workerUID)).Run() == nil
}
func shutdown() {
	time.Sleep(3 * time.Second)
	_ = exec.Command("/sbin/shutdown", "-h", "now").Run()
}
func serve() error {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(stateDir, "service.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return invalid
	}
	defer shutdown()
	if _, e := os.Stat(filepath.Join(stateDir, "intent")); e == nil {
		for runningWorker() {
			time.Sleep(time.Second)
		}
		return nil
	}
	// Bootstrap creates this service only after preparing the environment.
	// Bound the interval awaiting the finite envelope, never an executing worker.
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		if _, e := os.Stat(filepath.Join(stateDir, "fingerprint")); e == nil {
			f, e := os.Open(filepath.Join(stateDir, "launch.json"))
			if e != nil {
				return invalid
			}
			envelope, e := decode(f)
			f.Close()
			if e != nil {
				return e
			}
			_, e = execute(envelope, stateDir, workerUID, workerGID)
			return e
		}
		time.Sleep(250 * time.Millisecond)
	}
	return errors.New("delivery deadline expired")
}
func main() {
	var err error
	if len(os.Args) == 3 && os.Args[1] == "docker-relay" {
		err = serveDockerRelay(os.Args[2])
	} else if len(os.Args) != 2 {
		err = invalid
	} else {
		switch os.Args[1] {
		case "version":
			fmt.Println("pulse-tart-guest 0.1.0")
			return
		case "ingest":
			err = ingest(stateDir, os.Stdin)
		case "serve":
			err = serve()
		default:
			err = invalid
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "guest operation failed")
		os.Exit(1)
	}
}
