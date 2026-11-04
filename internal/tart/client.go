// Package tart implements local native operations using Tart and SSH.
package tart

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/colony-2/pulse-tart/internal/backend"
	"github.com/colony-2/pulse-tart/internal/config"
	"github.com/colony-2/pulse-tart/internal/ctxlock"
	"golang.org/x/crypto/ssh"
)

var Err = errors.New("Tart operation failed")
var ErrPlatform = errors.New("direct Tart requires macOS ARM64")

type command func(context.Context, ...string) ([]byte, error)
type record struct {
	Spec      backend.Spec `json:"spec"`
	UID       string       `json:"uid"`
	Phase     string       `json:"phase"`
	PID       int          `json:"pid"`
	Device    uint64       `json:"device"`
	Inode     uint64       `json:"inode"`
	MAC       string       `json:"mac"`
	HostKey   []byte       `json:"host_key,omitempty"`
	Created   time.Time    `json:"created"`
	Delivered bool         `json:"delivered"`
}
type hardware struct {
	CPU        int64
	Memory     int64
	MACAddress string
	State      string
	Running    bool
	Name       string
}
type Client struct {
	mu      ctxlock.Mutex
	cfg     config.Tart
	dir     string
	bases   []config.Base
	timeout time.Duration
	command command
	launch  func(string) (int, error)
	records map[string]*record
	ctx     context.Context
	cancel  context.CancelFunc
	workers map[string]bool
	wg      sync.WaitGroup
	closed  bool
}

func New(c config.Tart, common config.Config) (*Client, error) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return nil, ErrPlatform
	}
	if c.Binary == "" {
		c.Binary = "tart"
	}
	binary, e := exec.LookPath(c.Binary)
	if e != nil {
		return nil, Err
	}
	c.Binary, e = filepath.Abs(binary)
	if e != nil {
		return nil, Err
	}
	if c.Home == "" {
		home, e := os.UserHomeDir()
		if e != nil {
			return nil, Err
		}
		c.Home = filepath.Join(home, ".tart")
	}
	if !filepath.IsAbs(c.Home) {
		return nil, config.Err
	}
	c.Home = filepath.Clean(c.Home)
	if c.CommandTimeout == 0 {
		c.CommandTimeout = 2 * time.Minute
	}
	if c.SSHTimeout == 0 {
		c.SSHTimeout = 15 * time.Second
	}
	if c.CommandTimeout < time.Second || c.SSHTimeout < time.Second {
		return nil, config.Err
	}
	client, e := open(c, common)
	if e != nil {
		return nil, e
	}
	client.command = client.execute
	client.launch = client.start
	return client, nil
}
func open(c config.Tart, common config.Config) (*Client, error) {
	ctx, cancel := context.WithCancel(context.Background())
	b := &Client{cfg: c, dir: filepath.Join(common.StateDir, "native"), bases: common.Bases, timeout: common.BootstrapTimeout, records: map[string]*record{}, workers: map[string]bool{}, ctx: ctx, cancel: cancel}
	fail := func() (*Client, error) { cancel(); return nil, Err }
	if os.MkdirAll(b.dir, 0700) != nil {
		return fail()
	}
	st, e := os.Lstat(b.dir)
	if e != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return fail()
	}
	entries, e := os.ReadDir(b.dir)
	if e != nil {
		return fail()
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, e := os.ReadFile(filepath.Join(b.dir, entry.Name()))
		if e != nil {
			return fail()
		}
		var r record
		if json.Unmarshal(data, &r) != nil || !config.Name(r.Spec.Name) || entry.Name() != r.Spec.Name+".json" || r.UID == "" {
			return fail()
		}
		b.records[r.Spec.Name] = &r
	}
	return b, nil
}
func (b *Client) Home() string { return b.cfg.Home }
func (b *Client) env() []string {
	out := []string{}
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "TART_HOME=") && !strings.HasPrefix(v, "TART_NO_AUTO_PRUNE=") {
			out = append(out, v)
		}
	}
	return append(out, "TART_HOME="+b.cfg.Home, "TART_NO_AUTO_PRUNE=true")
}

type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 4<<20 {
		return 0, Err
	}
	return b.Buffer.Write(p)
}
func (b *Client) execute(ctx context.Context, args ...string) ([]byte, error) {
	timeout := b.cfg.CommandTimeout
	if len(args) > 0 && args[0] == "clone" {
		timeout = b.timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.cfg.Binary, args...)
	cmd.Env = b.env()
	var out boundedBuffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if cmd.Run() != nil {
		return nil, Err
	}
	return out.Bytes(), nil
}
func (b *Client) save(r *record) error {
	data, e := json.Marshal(r)
	if e != nil {
		return Err
	}
	f, e := os.CreateTemp(b.dir, ".pending-")
	if e != nil {
		return Err
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(data); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e == nil {
		e = os.Rename(f.Name(), filepath.Join(b.dir, r.Spec.Name+".json"))
	}
	if e == nil {
		d, de := os.Open(b.dir)
		if de != nil {
			return Err
		}
		e = d.Sync()
		d.Close()
	}
	if e != nil {
		return Err
	}
	return nil
}
func (b *Client) path(name string) string { return filepath.Join(b.cfg.Home, "vms", name) }
func (b *Client) binding(r *record, claim bool) error {
	st, e := os.Lstat(b.path(r.Spec.Name))
	if e != nil {
		return Err
	}
	if !st.IsDir() {
		return Err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return Err
	}
	marker := filepath.Join(b.path(r.Spec.Name), ".pulse-owner")
	if claim {
		if os.WriteFile(marker, []byte(r.UID), 0600) != nil {
			return Err
		}
		r.Device = uint64(sys.Dev)
		r.Inode = uint64(sys.Ino)
		return nil
	}
	got, e := os.ReadFile(marker)
	if e != nil || string(got) != r.UID || r.Device != uint64(sys.Dev) || r.Inode != uint64(sys.Ino) {
		return Err
	}
	return nil
}
func (b *Client) inspect(ctx context.Context, name string) (hardware, error) {
	var h hardware
	data, e := b.command(ctx, "get", name, "--format", "json")
	if e != nil {
		return h, e
	}
	if json.Unmarshal(data, &h) != nil || h.State == "" {
		return h, Err
	}
	return h, nil
}
func (b *Client) Info(ctx context.Context) error { _, e := b.command(ctx, "--version"); return e }
func (b *Client) runExited(r *record) bool {
	_, err := os.Stat(filepath.Join(b.dir, r.Spec.Name+".exit"))
	return err == nil || (r.PID > 0 && syscall.Kill(r.PID, 0) == syscall.ESRCH)
}
func (b *Client) get(ctx context.Context, name string) (backend.VM, error) {
	r := b.records[name]
	if r == nil {
		return backend.VM{}, &backend.Error{Code: 404}
	}
	if _, e := os.Lstat(b.path(name)); os.IsNotExist(e) {
		// Before run is dispatched, absence is uncertain, not deletion evidence.
		if r.Phase == "creating" {
			return backend.VM{}, Err
		}
		return backend.VM{}, &backend.Error{Code: 404}
	}
	if b.binding(r, false) != nil {
		return backend.VM{}, Err
	}
	h, e := b.inspect(ctx, name)
	if e != nil || h.CPU != r.Spec.CPU || h.Memory != r.Spec.Memory || h.MACAddress != r.MAC {
		return backend.VM{}, Err
	}
	v := backend.VM{Spec: r.Spec, UID: r.UID, Worker: "local", LocalName: name, CreatedAt: r.Created, PowerState: h.State, Generation: 1, ObservedGeneration: 1, Status: "pending"}
	running := "false"
	switch h.State {
	case "running":
		v.Status = "running"
		running = "true"
	case "suspended":
		v.Status = "running"
	case "stopped":
		if b.runExited(r) || r.Phase == "prepared" {
			v.Status = "failed"
		} else {
			running = "true"
		}
	default:
		return backend.VM{}, Err
	}
	v.Conditions = []backend.Condition{{Type: "running", State: running}}
	return v, nil
}
func (b *Client) Get(ctx context.Context, name string) (backend.VM, error) {
	if e := b.mu.LockContext(ctx); e != nil {
		return backend.VM{}, e
	}
	defer b.mu.Unlock()
	return b.get(ctx, name)
}
func (b *Client) List(ctx context.Context) ([]backend.VM, error) {
	if e := b.mu.LockContext(ctx); e != nil {
		return nil, e
	}
	defer b.mu.Unlock()
	data, e := b.command(ctx, "list", "--source", "local", "--format", "json")
	if e != nil {
		return nil, Err
	}
	var inventory []hardware
	if json.Unmarshal(data, &inventory) != nil || inventory == nil {
		return nil, Err
	}
	out := []backend.VM{}
	for _, h := range inventory {
		if b.records[h.Name] == nil {
			out = append(out, backend.VM{Spec: backend.Spec{Name: h.Name}, UID: "foreign:" + h.Name})
			continue
		}
		v, e := b.get(ctx, h.Name)
		if e != nil {
			return nil, Err
		}
		out = append(out, v)
	}
	return out, nil
}
func (b *Client) Create(ctx context.Context, s backend.Spec) (backend.VM, error) {
	if e := b.mu.LockContext(ctx); e != nil {
		return backend.VM{}, e
	}
	defer b.mu.Unlock()
	if b.closed || ctx.Err() != nil || !config.Name(s.Name) || b.records[s.Name] != nil {
		return backend.VM{}, Err
	}
	if _, e := os.Lstat(b.path(s.Name)); !os.IsNotExist(e) {
		return backend.VM{}, Err
	}
	var key []byte
	template := strings.Contains(s.Image, "@sha256:")
	if template {
		found := false
		for _, base := range b.bases {
			if base.Image == s.Image {
				key = base.SSHHostKey
				found = true
				break
			}
		}
		if !found {
			return backend.VM{}, Err
		}
	} else {
		parent := b.records[s.Image]
		if parent == nil {
			return backend.VM{}, Err
		}
		v, e := b.get(ctx, s.Image)
		if e != nil || v.PowerState != "suspended" {
			return backend.VM{}, Err
		}
		key = parent.HostKey
		data, e := b.command(ctx, "list", "--source", "local", "--format", "json")
		if e != nil {
			return backend.VM{}, Err
		}
		var inventory []hardware
		if json.Unmarshal(data, &inventory) != nil {
			return backend.VM{}, Err
		}
		for _, vm := range inventory {
			if vm.State != "running" && !vm.Running {
				continue
			}
			h, e := b.inspect(ctx, vm.Name)
			if e != nil || h.MACAddress == parent.MAC {
				return backend.VM{}, Err
			}
		}
	}
	var uid [16]byte
	if _, e := rand.Read(uid[:]); e != nil {
		return backend.VM{}, Err
	}
	r := &record{Spec: s, UID: hex.EncodeToString(uid[:]), Phase: "creating", Created: time.Now().UTC(), HostKey: key}
	b.records[s.Name] = r
	if b.save(r) != nil {
		return backend.VM{}, Err
	}
	if _, e := b.command(ctx, "clone", s.Image, s.Name); e != nil {
		return backend.VM{}, Err
	}
	if template {
		if _, e := b.command(ctx, "set", s.Name, "--cpu", strconv.FormatInt(s.CPU, 10), "--memory", strconv.FormatInt(s.Memory, 10), "--random-mac"); e != nil {
			return backend.VM{}, Err
		}
	}
	h, e := b.inspect(ctx, s.Name)
	if e != nil || h.CPU != s.CPU || h.Memory != s.Memory || h.MACAddress == "" {
		return backend.VM{}, Err
	}
	r.MAC = h.MACAddress
	if b.binding(r, true) != nil {
		return backend.VM{}, Err
	}
	r.Phase = "prepared"
	if b.save(r) != nil {
		return backend.VM{}, Err
	}
	r.Phase = "starting"
	if b.save(r) != nil {
		return backend.VM{}, Err
	}
	pid, e := b.launch(s.Name)
	if e != nil {
		return backend.VM{}, Err
	}
	r.PID = pid
	r.Phase = "started"
	if b.save(r) != nil {
		return backend.VM{}, Err
	}
	b.deliver(r)
	return backend.VM{Spec: s, UID: r.UID, Worker: "local", LocalName: s.Name, Status: "pending", CreatedAt: r.Created}, nil
}

// The detached wrapper writes a durable exit observation even if the Go host
// exits. Neither argv nor inherited environment contains the launch payload.
func (b *Client) start(name string) (int, error) {
	f, e := os.OpenFile(filepath.Join(b.dir, name+".run.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return 0, Err
	}
	cmd := exec.Command("/bin/sh", "-c", `"$1" run --no-graphics --suspendable "$2"
code=$?
umask 077
printf '%s\n' "$code" > "$3.tmp"
mv "$3.tmp" "$3"
`, "pulse-tart-run", b.cfg.Binary, name, filepath.Join(b.dir, name+".exit"))
	cmd.Env = b.env()
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() != nil {
		f.Close()
		return 0, Err
	}
	f.Close()
	go func() { _ = cmd.Wait() }()
	return cmd.Process.Pid, nil
}
func (b *Client) Suspend(ctx context.Context, name, uid string) error {
	if e := b.mu.LockContext(ctx); e != nil {
		return e
	}
	defer b.mu.Unlock()
	v, e := b.get(ctx, name)
	if e != nil || v.UID != uid {
		return Err
	}
	if v.PowerState == "suspended" {
		return nil
	}
	if v.PowerState != "running" {
		return Err
	}
	_, e = b.command(ctx, "suspend", name)
	return e
}
func (b *Client) Delete(ctx context.Context, name string) error {
	if e := b.mu.LockContext(ctx); e != nil {
		return e
	}
	defer b.mu.Unlock()
	v, e := b.get(ctx, name)
	if e != nil {
		return e
	}
	if !v.Stopped() || b.workers[name] {
		return Err
	}
	if _, e = b.command(ctx, "delete", name); e != nil {
		return e
	}
	if _, e = os.Lstat(b.path(name)); !os.IsNotExist(e) {
		return Err
	}
	if os.Remove(filepath.Join(b.dir, name+".json")) != nil {
		return Err
	}
	delete(b.records, name)
	for _, suffix := range []string{".run.log", ".events", ".exit"} {
		_ = os.Remove(filepath.Join(b.dir, name+suffix))
	}
	return nil
}
func (b *Client) Events(ctx context.Context, name string) ([]backend.Event, error) {
	if e := b.mu.LockContext(ctx); e != nil {
		return nil, e
	}
	defer b.mu.Unlock()
	if ctx.Err() != nil || b.records[name] == nil {
		return nil, Err
	}
	f, e := os.Open(filepath.Join(b.dir, name+".events"))
	if os.IsNotExist(e) {
		return []backend.Event{}, nil
	}
	if e != nil {
		return nil, Err
	}
	defer f.Close()
	// Startup output is fixed markers; bound even unexpected SSH output.
	st, e := f.Stat()
	if e != nil {
		return nil, Err
	}
	if st.Size() > 1<<20 {
		_, _ = f.Seek(-(1 << 20), io.SeekEnd)
	}
	data, e := io.ReadAll(io.LimitReader(f, 1<<20))
	if e != nil {
		return nil, Err
	}
	return []backend.Event{{Payload: string(data)}}, nil
}

// Recover resumes only idempotent guest delivery, never clone or run. List/Get
// remain observational and never start a delivery worker.
func (b *Client) Recover(ctx context.Context) error {
	if e := b.mu.LockContext(ctx); e != nil {
		return e
	}
	defer b.mu.Unlock()
	if b.closed {
		return Err
	}
	for name, r := range b.records {
		if r.Phase != "started" || r.Delivered || b.workers[name] {
			continue
		}
		v, e := b.get(ctx, name)
		if e != nil {
			if backend.IsStatus(e, 404) {
				continue
			}
			return e
		}
		if v.PowerState == "running" || v.Status == "pending" || (v.PowerState == "suspended" && !b.runExited(r)) {
			b.deliver(r)
		}
	}
	return nil
}
func (b *Client) deliver(r *record) {
	if b.workers[r.Spec.Name] || b.closed || r.Delivered {
		return
	}
	b.workers[r.Spec.Name] = true
	b.wg.Add(1)
	// Copy immutable data before starting; journal/worker flags are under mu.
	spec := r.Spec
	key := bytes.Clone(r.HostKey)
	name := spec.Name
	go func() {
		defer b.wg.Done()
		defer func() { b.mu.Lock(); delete(b.workers, name); b.mu.Unlock() }()
		ctx, cancel := context.WithTimeout(b.ctx, b.timeout)
		defer cancel()
		for ctx.Err() == nil {
			b.mu.Lock()
			v, err := b.get(ctx, name)
			// A fresh clone retains its snapshot's suspended state until the
			// detached Tart process resumes it. Only a finished run proves that
			// suspension is terminal and delivery should stop.
			finished := err == nil && ((v.Status == "failed" && v.Stopped()) || (v.PowerState == "suspended" && b.runExited(r)))
			b.mu.Unlock()
			if backend.IsStatus(err, 404) || finished {
				return
			}
			if err == nil && v.PowerState == "running" {
				data, e := b.command(ctx, "ip", name, "--wait", "5")
				ip := strings.TrimSpace(string(data))
				if e == nil && net.ParseIP(ip) != nil {
					e = b.send(ctx, ip, spec, key)
					events, _ := os.ReadFile(filepath.Join(b.dir, name+".events"))
					if e == nil || bytes.Contains(events, []byte("PULSE_HANDOFF_OK")) {
						b.mu.Lock()
						r.Delivered = true
						_ = b.save(r)
						b.mu.Unlock()
						return
					}
				}
			}
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}
func (b *Client) send(ctx context.Context, ip string, spec backend.Spec, key []byte) error {
	return b.sendAddress(ctx, net.JoinHostPort(ip, "22"), spec, key)
}
func (b *Client) sendAddress(ctx context.Context, address string, spec backend.Spec, key []byte) error {
	callback := ssh.InsecureIgnoreHostKey()
	if len(key) > 0 {
		pub, _, _, _, e := ssh.ParseAuthorizedKey(key)
		if e != nil {
			return Err
		}
		callback = ssh.FixedHostKey(pub)
	}
	cfg := &ssh.ClientConfig{User: spec.Username, Auth: []ssh.AuthMethod{ssh.Password(spec.Password)}, HostKeyCallback: callback, Timeout: b.cfg.SSHTimeout}
	conn, e := (&net.Dialer{Timeout: b.cfg.SSHTimeout}).DialContext(ctx, "tcp", address)
	if e != nil {
		return Err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(b.cfg.SSHTimeout))
	cc, ch, req, e := ssh.NewClientConn(conn, address, cfg)
	if e != nil {
		return Err
	}
	_ = conn.SetDeadline(time.Time{})
	client := ssh.NewClient(cc, ch, req)
	defer client.Close()
	session, e := client.NewSession()
	if e != nil {
		return Err
	}
	defer session.Close()
	f, e := os.OpenFile(filepath.Join(b.dir, spec.Name+".events"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if e != nil {
		return Err
	}
	defer f.Close()
	session.Stdin = strings.NewReader(spec.StartupScript.Content)
	session.Stdout = f
	session.Stderr = f
	if session.Run("/bin/bash -s") != nil {
		return Err
	}
	return nil
}
func (b *Client) Close() error {
	b.mu.Lock()
	b.closed = true
	b.cancel()
	b.mu.Unlock()
	b.wg.Wait()
	return nil
}

// String intentionally excludes SSH credentials and private delivery records.
func (b *Client) String() string { return fmt.Sprintf("Tart(%s)", b.cfg.Home) }
