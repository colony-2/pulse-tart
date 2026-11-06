package docker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	api "github.com/colony-2/pulse-provider-api"
	"golang.org/x/crypto/ssh"
)

func testSSH(t *testing.T, handle func(string, []byte) string) (string, []byte, []byte) {
	t.Helper()
	_, clientKey, _ := ed25519.GenerateKey(rand.Reader)
	clientSigner, _ := ssh.NewSignerFromKey(clientKey)
	_, hostKey, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostKey)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	rsaSigner, _ := ssh.NewSignerFromKey(rsaKey)
	cfg := &ssh.ServerConfig{PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) { return nil, nil }, PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if bytes.Equal(key.Marshal(), clientSigner.PublicKey().Marshal()) {
			return nil, nil
		}
		return nil, os.ErrPermission
	}}
	// Server offers RSA first; a client pinning ED25519 must negotiate ED25519.
	cfg.AddHostKey(rsaSigner)
	cfg.AddHostKey(hostSigner)
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, e := listener.Accept()
			if e != nil {
				return
			}
			go func() {
				c, channels, requests, e := ssh.NewServerConn(conn, cfg)
				if e != nil {
					conn.Close()
					return
				}
				defer c.Close()
				go ssh.DiscardRequests(requests)
				for ch := range channels {
					stream, reqs, e := ch.Accept()
					if e != nil {
						return
					}
					go func(kind string) {
						defer stream.Close()
						if kind != "session" {
							go ssh.DiscardRequests(reqs)
							io.Copy(stream, stream)
							return
						}
						for req := range reqs {
							if req.Type != "exec" {
								req.Reply(false, nil)
								continue
							}
							var command struct{ Command string }
							ssh.Unmarshal(req.Payload, &command)
							req.Reply(true, nil)
							body, _ := io.ReadAll(stream)
							io.WriteString(stream, handle(command.Command, body))
							stream.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
							return
						}
					}(ch.ChannelType())
				}
			}()
		}
	}()
	key, _ := x509.MarshalPKCS8PrivateKey(clientKey)
	return listener.Addr().String(), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), ssh.MarshalAuthorizedKey(hostSigner.PublicKey())
}
func TestPinnedSSHNegotiatesMatchingHostAlgorithm(t *testing.T) {
	addr, key, host := testSSH(t, func(string, []byte) string { return "" })
	dial, closeAll, e := SSHTransport(t.Context(), addr, "engine", key, host)
	if e != nil {
		t.Fatal(e)
	}
	defer closeAll()
	c, e := dial(t.Context(), "unix", "/var/run/docker.sock")
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	if _, e = c.Write([]byte("ping")); e != nil {
		t.Fatal(e)
	}
	b := make([]byte, 4)
	if _, e = io.ReadFull(c, b); e != nil || string(b) != "ping" {
		t.Fatal(string(b), e)
	}
}
func TestSuppliedLeaseSurvivesHandoffAndReopenDoesNotExecuteTwice(t *testing.T) {
	var starts atomic.Int32
	var received []byte
	addr, _, _ := testSSH(t, func(command string, body []byte) string {
		if strings.Contains(command, "accept.py") {
			starts.Add(1)
			received = append([]byte(nil), body...)
			return "PULSE_DOCKER_ACCEPTED\n"
		}
		return "00000000-0000-4000-8000-000000000001\nengine-id\n"
	})
	cfg := Config{Enabled: true, StateDir: t.TempDir(), NativeID: "immutable-native-id", Base: "ubuntu-24.04-arm64", Image: "registry/image@sha256:" + strings.Repeat("a", 64), CPU: 4, MemoryMiB: 7680, Guest: Guest{VM: "owned-vm", Address: addr, User: "test", Password: "test"}}
	p, e := NewLinux(t.Context(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	lease := []byte{0, 255, 254, 10}
	l := api.Launch{LaunchID: "test", Allocation: api.Allocation{Base: "ubuntu-*arm64", Capabilities: []string{"docker"}}, Job: api.JobRef{JobDB: "http://jobdb.invalid", JobID: "job", TenantID: "tenant", InstanceID: "instance"}, Lease: api.Lease(lease)}
	out, e := p.Submit(t.Context(), []api.Launch{l})
	if e != nil || out[0].Status != api.Accepted {
		t.Fatal(out, e)
	}
	p.Close()
	p, e = NewLinux(t.Context(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	out, e = p.Submit(t.Context(), []api.Launch{l})
	if e != nil || out[0].Status != api.Accepted || starts.Load() != 1 {
		t.Fatal(out, e, starts.Load())
	}
	var payload struct {
		Lease string
		Args  []string
	}
	if json.Unmarshal(received, &payload) != nil {
		t.Fatal("bad envelope")
	}
	got, e := base64.StdEncoding.DecodeString(payload.Lease)
	if e != nil || !bytes.Equal(got, lease) {
		t.Fatal("lease bytes changed")
	}
	if strings.Contains(strings.Join(payload.Args, " "), payload.Lease) {
		t.Fatal("lease entered argv")
	}
	l.LaunchID = "other"
	out, e = p.Submit(t.Context(), []api.Launch{l})
	if e != nil || out[0].Status != api.NoCapacity {
		t.Fatal(out, e)
	}
}
func TestUnknownReservationIsNotRetriedWhileBackendIsUnreachable(t *testing.T) {
	l := api.Launch{LaunchID: "unknown", Allocation: api.Allocation{Base: "ubuntu-*"}, Job: api.JobRef{JobDB: "http://jobdb.invalid", JobID: "job", TenantID: "tenant", InstanceID: "instance"}, Lease: "opaque"}
	without := l
	without.Lease = ""
	b, _ := json.Marshal(struct {
		Launch api.Launch
		Lease  []byte
	}{without, []byte(l.Lease)})
	sum := sha256.Sum256(b)
	p := &Provider{cfg: Config{Base: "ubuntu-24.04-arm64", CPU: 4, MemoryMiB: 7680}, entry: &entry{ID: l.LaunchID, Hash: hex.EncodeToString(sum[:]), Status: api.Unknown}}
	out, e := p.Submit(context.Background(), []api.Launch{l})
	if e != nil || out[0].Status != api.Unknown {
		t.Fatal(out, e)
	}
}
func TestDisabledPrototypeDoesNotCreateState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if _, e := NewLinux(t.Context(), Config{StateDir: root}); e == nil {
		t.Fatal("disabled prototype accepted")
	}
	if _, e := os.Stat(root); !os.IsNotExist(e) {
		t.Fatal("disabled prototype changed disk")
	}
}

func TestMalformedAndUnsupportedLaunchesDoNotReserve(t *testing.T) {
	p := &Provider{cfg: Config{Base: "ubuntu-24.04-arm64", CPU: 4, MemoryMiB: 7680}}
	valid := api.Launch{LaunchID: "test", Allocation: api.Allocation{Base: "ubuntu-*"}, Job: api.JobRef{JobDB: "http://jobdb.invalid", JobID: "job", TenantID: "tenant", InstanceID: "instance"}, Lease: "opaque"}
	for _, tc := range []struct {
		name   string
		modify func(*api.Launch)
		status api.Status
	}{
		{"missing job identity", func(l *api.Launch) { l.Job.TenantID = "" }, api.Rejected},
		{"credentials in URL", func(l *api.Launch) { l.Job.JobDB = "https://user:secret@jobdb.invalid" }, api.Rejected},
		{"quantity overflow", func(l *api.Launch) { l.CPUMillis = api.MaxQuantity + 1 }, api.Rejected},
		{"wrong base", func(l *api.Launch) { l.Base = "macos*" }, api.Unsupported},
		{"unsupported capability", func(l *api.Launch) { l.Capabilities = []string{"gpu"} }, api.Unsupported},
		{"unsupported timeout", func(l *api.Launch) { l.TimeoutSeconds = 60 }, api.Unsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := valid
			tc.modify(&l)
			out, err := p.Submit(t.Context(), []api.Launch{l})
			if err != nil || out[0].Status != tc.status || p.entry != nil {
				t.Fatal(out, err)
			}
		})
	}
}
func TestUnreachableObservationIsError(t *testing.T) {
	p := &Provider{entry: &entry{ID: "reserved"}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.List(ctx, api.ListRequest{}); err == nil {
		t.Fatal("unreachable allocation returned successful inventory")
	}
}

func TestMacOSAllocationVerifiesShareAndPreservesPrimaryFacts(t *testing.T) {
	var starts atomic.Int32
	var checks atomic.Int32
	var delivery atomic.Value
	addr, _, _ := testSSH(t, func(command string, body []byte) string {
		if strings.Contains(command, "Linux/aarch64") {
			return "private-engine-id\n"
		}
		if strings.Contains(command, "check-workspace.py") {
			checks.Add(1)
			return ""
		}
		if strings.Contains(command, "accept.py") {
			starts.Add(1)
			delivery.Store(append([]byte(nil), body...))
			return "PULSE_DOCKER_ACCEPTED\n"
		}
		return "boot-id\nprivate-engine-id\n"
	})
	guest := Guest{VM: "primary", Address: addr, User: "admin", Password: "test"}
	engine := guest
	engine.VM = "companion"
	cfg := Config{Enabled: true, StateDir: t.TempDir(), NativeID: "mac-uid", Base: "macos-tahoe-26.6.2-arm64", Image: "registry/image@sha256:" + strings.Repeat("a", 64), CPU: 4, MemoryMiB: 7680, Guest: guest, Engine: engine, EngineNativeID: "engine-uid", Workspace: "/Users/Shared/pulse/work/jobs/owned"}
	p, err := NewMacOS(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if checks.Load() != 3 {
		t.Fatal("missing shared workspace round-trip", checks.Load())
	}
	before := checks.Load()
	caps, err := p.GetCapabilities(t.Context())
	if err != nil || caps.Bases[0].Name != cfg.Base || checks.Load() != before {
		t.Fatal(caps, err)
	}
	l := api.Launch{LaunchID: "mac-job", Allocation: api.Allocation{Base: "macos-*", Capabilities: []string{"docker"}}, Job: api.JobRef{JobDB: "http://jobdb.invalid", JobID: "job", TenantID: "tenant", InstanceID: "instance"}, Lease: "opaque"}
	result, err := p.Submit(t.Context(), []api.Launch{l})
	if err != nil || result[0].Status != api.Accepted {
		t.Fatal(result, err)
	}
	var payload struct {
		Args      []string
		Workspace string
	}
	if err = json.Unmarshal(delivery.Load().([]byte), &payload); err != nil {
		t.Fatal(err)
	}
	argv := strings.Join(payload.Args, " ")
	for _, fact := range []string{"--execution-platform darwin/arm64", "--execution-base " + cfg.Base, "--execution-cpu 4000m", "--execution-memory 7680Mi"} {
		if !strings.Contains(argv, fact) {
			t.Fatal("missing primary fact", fact, argv)
		}
	}
	if payload.Workspace != cfg.Workspace {
		t.Fatal(payload.Workspace)
	}
	listed, err := p.List(t.Context(), api.ListRequest{})
	if err != nil || len(listed.Items) != 2 {
		t.Fatal(listed, err)
	}
	for _, item := range listed.Items {
		if item.Job != l.Job || item.LaunchID != l.LaunchID {
			t.Fatal(item)
		}
	}
	p.Close()
	p, err = NewMacOS(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	result, err = p.Submit(t.Context(), []api.Launch{l})
	if err != nil || result[0].Status != api.Accepted || starts.Load() != 1 {
		t.Fatal(result, err, starts.Load())
	}
}

func TestMacOSAllocationRejectsWorkspaceEscapeBeforeSideEffects(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	_, err := NewMacOS(t.Context(), Config{Enabled: true, StateDir: root, Workspace: "/Users/Shared/pulse/work/jobs/../../other", EngineNativeID: "engine", Engine: Guest{VM: "engine"}})
	if err == nil {
		t.Fatal("accepted escaping workspace")
	}
	if _, err = os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("invalid config created state")
	}
}
