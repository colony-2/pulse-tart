package tart

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"github.com/colony-2/pulse-tart/internal/backend"
	"github.com/colony-2/pulse-tart/internal/config"
	"golang.org/x/crypto/ssh"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type mockNative struct {
	mu        sync.Mutex
	home      string
	vms       map[string]hardware
	calls     [][]string
	ambiguous bool
}

func (m *mockNative) run(ctx context.Context, args ...string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	m.calls = append(m.calls, append([]string{}, args...))
	out := any(nil)
	switch args[0] {
	case "clone":
		h, ok := m.vms[args[1]]
		if !ok {
			h = hardware{CPU: 2, Memory: 4096, MACAddress: "02:00:00:00:00:01", State: "stopped"}
		}
		h.Name = args[2]
		m.vms[h.Name] = h
		if e := os.MkdirAll(filepath.Join(m.home, "vms", h.Name), 0700); e != nil {
			return nil, e
		}
		if m.ambiguous {
			return nil, errors.New("sensitive command error")
		}
	case "set":
		h := m.vms[args[1]]
		h.CPU, _ = strconv.ParseInt(args[3], 10, 64)
		h.Memory, _ = strconv.ParseInt(args[5], 10, 64)
		h.MACAddress = "02:00:00:00:00:02"
		m.vms[args[1]] = h
	case "get":
		h, ok := m.vms[args[1]]
		if !ok {
			return nil, Err
		}
		out = h
	case "list":
		all := []hardware{}
		for _, h := range m.vms {
			all = append(all, h)
		}
		out = all
	case "suspend":
		h := m.vms[args[1]]
		h.State = "suspended"
		m.vms[args[1]] = h
	case "delete":
		delete(m.vms, args[1])
		return nil, os.RemoveAll(filepath.Join(m.home, "vms", args[1]))
	case "ip":
		return nil, Err
	case "--version":
		return []byte("test"), nil
	default:
		return nil, Err
	}
	return json.Marshal(out)
}
func fixture(t *testing.T) (*Client, *mockNative, backend.Spec) {
	t.Helper()
	dir := t.TempDir()
	home := t.TempDir()
	common := config.Config{StateDir: dir, BootstrapTimeout: time.Minute, Bases: []config.Base{{Image: "example.test/macos@sha256:" + strings.Repeat("a", 64)}}}
	b, e := open(config.Tart{Home: home, CommandTimeout: time.Second, SSHTimeout: time.Second}, common)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { b.Close() })
	mock := &mockNative{home: home, vms: map[string]hardware{}}
	b.command = mock.run
	b.launch = func(name string) (int, error) {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		h := mock.vms[name]
		h.State = "running"
		mock.vms[name] = h
		return os.Getpid(), nil
	}
	return b, mock, backend.Spec{Name: "template", Image: common.Bases[0].Image, CPU: 4, Memory: 8192, Username: "admin", Password: "ssh-password", Suspendable: true, StartupScript: backend.Script{Content: "private opaque lease payload"}}
}
func TestTemplateAndSuspendedCloneKeepHardware(t *testing.T) {
	b, m, s := fixture(t)
	v, e := b.Create(t.Context(), s)
	if e != nil {
		t.Fatal(e)
	}
	if e = b.Suspend(t.Context(), s.Name, v.UID); e != nil {
		t.Fatal(e)
	}
	s.Name = "job"
	s.Image = "template"
	if _, e = b.Create(t.Context(), s); e != nil {
		t.Fatal(e)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sets := 0
	for _, args := range m.calls {
		if args[0] == "set" {
			sets++
			if args[1] != "template" {
				t.Fatal("resumed clone resized")
			}
		}
		if strings.Contains(strings.Join(args, " "), "opaque lease") {
			t.Fatal("lease in command argv")
		}
	}
	if sets != 1 || m.vms["job"].MACAddress != m.vms["template"].MACAddress {
		t.Fatal("snapshot identity changed")
	}
}
func TestMACCollisionAndAmbiguousCreateDoNotRetry(t *testing.T) {
	t.Run("collision", func(t *testing.T) {
		b, m, s := fixture(t)
		v, e := b.Create(t.Context(), s)
		if e != nil {
			t.Fatal(e)
		}
		if e = b.Suspend(t.Context(), s.Name, v.UID); e != nil {
			t.Fatal(e)
		}
		m.mu.Lock()
		h := m.vms[s.Name]
		h.Name = "foreign"
		h.State = "running"
		m.vms[h.Name] = h
		m.mu.Unlock()
		s.Name = "job"
		s.Image = "template"
		if _, e = b.Create(t.Context(), s); e == nil {
			t.Fatal("MAC collision accepted")
		}
		if b.records["job"] != nil {
			t.Fatal("collision mutated state")
		}
	})
	t.Run("unknown clone", func(t *testing.T) {
		b, m, s := fixture(t)
		m.ambiguous = true
		if _, e := b.Create(t.Context(), s); e != Err {
			t.Fatal("unsanitized error", e)
		}
		if _, e := b.Create(t.Context(), s); e == nil {
			t.Fatal("ambiguous create replayed")
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		n := 0
		for _, args := range m.calls {
			if args[0] == "clone" {
				n++
			}
		}
		if n != 1 {
			t.Fatal("clone replayed")
		}
	})
}
func TestIdentityAndRunningJobPreventDeletion(t *testing.T) {
	b, _, s := fixture(t)
	v, e := b.Create(t.Context(), s)
	if e != nil {
		t.Fatal(e)
	}
	if e = b.Delete(t.Context(), s.Name); e == nil {
		t.Fatal("running worker deleted")
	}
	if e = b.Suspend(t.Context(), s.Name, "replacement"); e == nil {
		t.Fatal("replacement UID suspended")
	}
	b.Close()
	if e = os.WriteFile(filepath.Join(b.path(s.Name), ".pulse-owner"), []byte("replacement"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = b.Get(t.Context(), s.Name); e == nil {
		t.Fatal("replacement VM matched")
	}
	if e = b.Delete(t.Context(), s.Name); e == nil {
		t.Fatal("replacement VM deleted")
	}
	if e = b.Suspend(t.Context(), s.Name, v.UID); e == nil {
		t.Fatal("replacement VM suspended")
	}
}
func TestReadOnlyInventoryAfterRestart(t *testing.T) {
	b, m, s := fixture(t)
	if _, e := b.Create(t.Context(), s); e != nil {
		t.Fatal(e)
	}
	b.Close()
	common := config.Config{StateDir: filepath.Dir(b.dir), BootstrapTimeout: time.Minute, Bases: b.bases}
	reopened, e := open(b.cfg, common)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	reopened.command = m.run
	if _, e = reopened.List(t.Context()); e != nil {
		t.Fatal(e)
	}
	if len(reopened.workers) != 0 {
		t.Fatal("List started guest delivery")
	}
	if e = reopened.Recover(t.Context()); e != nil {
		t.Fatal(e)
	}
	reopened.mu.Lock()
	workers := len(reopened.workers)
	reopened.mu.Unlock()
	if workers != 1 {
		t.Fatal("explicit recovery did not resume handoff")
	}
}
func TestDetachedRunSurvivesClose(t *testing.T) {
	b, _, _ := fixture(t)
	binary := filepath.Join(t.TempDir(), "tart-test")
	if e := os.WriteFile(binary, []byte("#!/bin/sh\ntest \"$1 $2 $3\" = 'run --no-graphics --suspendable' || exit 42\nsleep 1\n"), 0700); e != nil {
		t.Fatal(e)
	}
	b.cfg.Binary = binary
	if _, e := b.start("detached"); e != nil {
		t.Fatal(e)
	}
	before := time.Now()
	b.Close()
	if time.Since(before) > 500*time.Millisecond {
		t.Fatal("Close waited for VM")
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		data, e := os.ReadFile(filepath.Join(b.dir, "detached.exit"))
		if e == nil {
			if strings.TrimSpace(string(data)) != "0" {
				t.Fatal("detached run failed")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("detached VM wrapper did not finish after Close")
}

func TestSSHDeliveryUsesOnlyStdinAndPinsHostKey(t *testing.T) {
	b, _, spec := fixture(t)
	_, private, e := ed25519.GenerateKey(nil)
	if e != nil {
		t.Fatal(e)
	}
	signer, e := ssh.NewSignerFromKey(private)
	if e != nil {
		t.Fatal(e)
	}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	received := make(chan string, 1)
	server := &ssh.ServerConfig{PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		if meta.User() != "admin" || string(password) != "ssh-password" {
			return nil, Err
		}
		return nil, nil
	}}
	server.AddHostKey(signer)
	go func() {
		conn, e := listener.Accept()
		if e != nil {
			return
		}
		defer conn.Close()
		cc, channels, requests, e := ssh.NewServerConn(conn, server)
		if e != nil {
			return
		}
		defer cc.Close()
		go ssh.DiscardRequests(requests)
		for next := range channels {
			ch, reqs, e := next.Accept()
			if e != nil {
				return
			}
			go func() {
				defer ch.Close()
				for req := range reqs {
					if req.Type != "exec" {
						_ = req.Reply(false, nil)
						continue
					}
					var command struct{ Command string }
					if ssh.Unmarshal(req.Payload, &command) != nil || command.Command != "/bin/bash -s" {
						_ = req.Reply(false, nil)
						return
					}
					_ = req.Reply(true, nil)
					data, e := io.ReadAll(ch)
					if e != nil {
						return
					}
					received <- string(data)
					_, _ = ch.Write([]byte("PULSE_HANDOFF_OK\n"))
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
					return
				}
			}()
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	key := ssh.MarshalAuthorizedKey(signer.PublicKey())
	if e = b.sendAddress(ctx, listener.Addr().String(), spec, key); e != nil {
		t.Fatal(e)
	}
	select {
	case got := <-received:
		if got != spec.StartupScript.Content {
			t.Fatal("SSH payload changed")
		}
	case <-ctx.Done():
		t.Fatal("no SSH stdin")
	}
	data, e := os.ReadFile(filepath.Join(b.dir, spec.Name+".events"))
	if e != nil || strings.Contains(string(data), "opaque lease") {
		t.Fatal("lease leaked to startup events")
	}
}
func TestSSHHandshakeHonorsCancellation(t *testing.T) {
	b, _, spec := fixture(t)
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	go func() {
		c, e := listener.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(io.Discard, c)
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	if e = b.sendAddress(ctx, listener.Addr().String(), spec, nil); e == nil {
		t.Fatal("unfinished handshake succeeded")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("SSH ignored cancellation")
	}
}

// Tart's detached run can still report the cloned snapshot as suspended before
// it resumes. Delivery must wait for it, without requiring Recover to be called.
func TestDeliveryWaitsForSuspendedCloneToResume(t *testing.T) {
	b, m, s := fixture(t)
	ipRequested := make(chan struct{}, 1)
	var observedSnapshot sync.Once
	b.command = func(ctx context.Context, args ...string) ([]byte, error) {
		data, err := m.run(ctx, args...)
		if len(args) > 1 && args[1] == "job" {
			if args[0] == "get" {
				bStarted := false
				// The first get is Create's hardware inspection, before launch.
				m.mu.Lock()
				bStarted = m.vms["job"].Running
				m.mu.Unlock()
				if bStarted {
					observedSnapshot.Do(func() {
						m.mu.Lock()
						h := m.vms["job"]
						h.State = "running"
						m.vms["job"] = h
						m.mu.Unlock()
					})
				}
			}
			if args[0] == "ip" {
				select {
				case ipRequested <- struct{}{}:
				default:
				}
			}
		}
		return data, err
	}
	v, err := b.Create(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Suspend(t.Context(), s.Name, v.UID); err != nil {
		t.Fatal(err)
	}
	b.launch = func(name string) (int, error) {
		m.mu.Lock()
		h := m.vms[name]
		h.Running = true // process launched, but native State remains suspended
		m.vms[name] = h
		m.mu.Unlock()
		return os.Getpid(), nil
	}
	s.Name, s.Image = "job", "template"
	v, err = b.Create(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ipRequested:
	case <-time.After(3 * time.Second):
		t.Fatal("delivery abandoned suspended clone before resume")
	}
	if err = b.Suspend(t.Context(), s.Name, v.UID); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(b.dir, "job.exit"), []byte("0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		b.mu.Lock()
		active := b.workers["job"]
		b.mu.Unlock()
		if !active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("delivery did not stop after suspension completed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = b.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	active := b.workers["job"]
	b.mu.Unlock()
	if active {
		t.Fatal("recovery restarted delivery to completed suspended VM")
	}
}

func TestPublishedSourceReplacementDoesNotChangeExistingClone(t *testing.T) {
	b, m, s := fixture(t)
	source, err := b.Create(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Suspend(t.Context(), s.Name, source.UID); err != nil {
		t.Fatal(err)
	}
	if err = b.RetainSource(t.Context(), s.Name, source.UID); err != nil {
		t.Fatal(err)
	}
	old := s
	old.Name = "old-job"
	old.Image = s.Name
	if _, err = b.Create(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	// Model `tart clone --overwrite staged template`: a new directory, snapshot,
	// and MAC under the published name, while the previous job remains running.
	m.mu.Lock()
	before := m.vms[old.Name]
	if err = os.RemoveAll(b.path(s.Name)); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(b.path(s.Name), 0700); err != nil {
		t.Fatal(err)
	}
	replacement := m.vms[s.Name]
	replacement.MACAddress = "02:00:00:00:00:99"
	m.vms[s.Name] = replacement
	m.mu.Unlock()
	if _, err = b.Get(t.Context(), s.Name); err != nil {
		t.Fatal("published source rejected after replacement", err)
	}
	next := old
	next.Name = "new-job"
	if _, err = b.Create(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	if m.vms[old.Name] != before || m.vms[next.Name].MACAddress != replacement.MACAddress {
		t.Error("replacement affected the wrong clone")
	}
	m.mu.Unlock()
	if _, err = b.Get(t.Context(), old.Name); err != nil {
		t.Fatal("old clone lost its ownership binding", err)
	}
	// The relaxed source rules must never relax job-clone ownership.
	if err = os.Remove(filepath.Join(b.path(old.Name), ".pulse-owner")); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Get(t.Context(), old.Name); err == nil {
		t.Fatal("job ownership mismatch accepted")
	}
}

func TestAdoptExternalPreparedSourceAndColdClone(t *testing.T) {
	b, m, s := fixture(t)
	s.Name = "published-base"
	m.vms[s.Name] = hardware{Name: s.Name, CPU: 4, Memory: 8192, State: "stopped", MACAddress: "02:00:00:00:00:31"}
	if err := os.MkdirAll(b.path(s.Name), 0700); err != nil {
		t.Fatal(err)
	}
	source, err := b.AdoptSource(t.Context(), s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if source.PowerState != "stopped" {
		t.Fatal(source.PowerState)
	}
	slot := s
	slot.Name = "larger-slot"
	slot.Image = s.Name
	slot.CPU = 8
	slot.Memory = 16384
	if _, err = b.Create(t.Context(), slot); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.vms[slot.Name].CPU != 8 || m.vms[slot.Name].Memory != 16384 || m.vms[slot.Name].MACAddress == m.vms[s.Name].MACAddress {
		t.Fatal("stopped base did not produce independently configured slot")
	}
}

func TestDeletedReusableSourceCanBePreparedAgain(t *testing.T) {
	b, m, s := fixture(t)
	m.vms[s.Name] = hardware{Name: s.Name, CPU: s.CPU, Memory: s.Memory, State: "stopped", MACAddress: "02:00:00:00:00:31"}
	if err := os.MkdirAll(b.path(s.Name), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AdoptSource(t.Context(), s, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(b.path(s.Name)); err != nil {
		t.Fatal(err)
	}
	delete(m.vms, s.Name)
	if _, err := b.AdoptSource(t.Context(), s, nil); !backend.IsStatus(err, 404) {
		t.Fatal(err)
	}
	if _, err := b.Create(t.Context(), s); err != nil {
		t.Fatal("deleted source could not be rebuilt", err)
	}
}

func TestLinuxDetachedRunDoesNotRequestUnsupportedSuspension(t *testing.T) {
	b, _, _ := fixture(t)
	binary := filepath.Join(t.TempDir(), "tart-linux-test")
	if e := os.WriteFile(binary, []byte("#!/bin/sh\ntest \"$1 $2 $3\" = 'run --no-graphics linux' || exit 42\n"), 0700); e != nil {
		t.Fatal(e)
	}
	b.cfg.Binary = binary
	b.records["linux"] = &record{Spec: backend.Spec{OS: "linux"}}
	if _, e := b.start("linux"); e != nil {
		t.Fatal(e)
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		data, e := os.ReadFile(filepath.Join(b.dir, "linux.exit"))
		if e == nil {
			if strings.TrimSpace(string(data)) != "0" {
				t.Fatal("Linux requested suspension")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("run did not finish")
}

func TestForeignInventoryPreservesPowerWithoutTakingOwnership(t *testing.T) {
	b, m, _ := fixture(t)
	// Reproduce the qualification host: four leftover stopped templates and no
	// running VMs. They must not consume any of the two macOS running slots.
	for _, name := range []string{"old-1", "old-2", "old-3", "old-4"} {
		m.vms[name] = hardware{Name: name, State: "stopped"}
	}
	vms, err := b.List(t.Context())
	if err != nil || len(vms) != 4 {
		t.Fatal(vms, err)
	}
	for _, v := range vms {
		if !v.Stopped() || v.PowerState != "stopped" || v.UID != "foreign:"+v.Name {
			t.Fatalf("stopped template counted as active: %+v", v)
		}
		if err := b.Stop(t.Context(), v.Name, v.UID); err == nil {
			t.Fatal("foreign observation authorized stop")
		}
		if err := b.Delete(t.Context(), v.Name); err == nil {
			t.Fatal("foreign observation authorized deletion")
		}
	}
	for _, tc := range []struct {
		state   string
		running bool
		stopped bool
	}{
		{"running", true, false},
		{"running", false, false},
		{"suspended", false, true},
		{"stopped", true, false},
		{"suspended", true, false},
		{"", false, false},
		{"unrecognized", false, false},
	} {
		m.vms = map[string]hardware{"foreign": {Name: "foreign", State: tc.state, Running: tc.running}}
		vms, err := b.List(t.Context())
		if err != nil || len(vms) != 1 {
			t.Fatal(vms, err)
		}
		if vms[0].Stopped() != tc.stopped || vms[0].PowerState != tc.state {
			t.Fatalf("state=%q running=%v: %+v", tc.state, tc.running, vms[0])
		}
	}
	if len(b.records) != 0 {
		t.Fatal("inventory adopted foreign VMs")
	}
	for _, call := range m.calls {
		if call[0] != "list" {
			t.Fatalf("foreign inventory mutated native resources: %v", call)
		}
	}
}
