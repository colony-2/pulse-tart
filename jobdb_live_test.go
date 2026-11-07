package tart

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-tart/internal/backend"
	"github.com/colony-2/pulse-tart/internal/config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/websocket"
)

var errGuestDiagnostic = errors.New("guest diagnostic connection unavailable")

// Only fixed test commands use this channel. Lease responses remain in memory;
// errors never contain response bodies, passwords, or serialized assignments.
func testGuestExec(ctx context.Context, kind string, connection OrchardConfig, v backend.VM, command string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if kind == "orchard" {
		base, e := config.NormalizeURL(connection.URL, connection.AllowInsecureHTTP)
		if e != nil {
			return nil, errGuestDiagnostic
		}
		u, e := url.Parse(base + "/vms/" + url.PathEscape(v.Name) + "/exec")
		if e != nil {
			return nil, errGuestDiagnostic
		}
		origin := u.Scheme + "://" + u.Host
		if u.Scheme == "https" {
			u.Scheme = "wss"
		} else {
			u.Scheme = "ws"
		}
		q := url.Values{"command": {command}, "wait": {"5"}}
		u.RawQuery = q.Encode()
		cfg, e := websocket.NewConfig(u.String(), origin)
		if e != nil {
			return nil, errGuestDiagnostic
		}
		cfg.Dialer = &net.Dialer{Timeout: 10 * time.Second}
		if connection.Token != "" {
			req, _ := http.NewRequest("GET", origin, nil)
			req.SetBasicAuth(connection.Username, string(connection.Token))
			cfg.Header = req.Header
		}
		ws, e := websocket.DialConfig(cfg)
		if e != nil {
			return nil, errGuestDiagnostic
		}
		defer ws.Close()
		stop := context.AfterFunc(ctx, func() { ws.Close() })
		defer stop()
		ws.MaxPayloadBytes = 1 << 20
		deadline, _ := ctx.Deadline()
		_ = ws.SetDeadline(deadline)
		var out bytes.Buffer
		for {
			var f struct {
				Type string `json:"type"`
				Data []byte `json:"data"`
				Exit *struct {
					Code int `json:"code"`
				} `json:"exit"`
			}
			if websocket.JSON.Receive(ws, &f) != nil {
				return nil, errGuestDiagnostic
			}
			switch f.Type {
			case "stdout":
				if out.Len()+len(f.Data) > 256<<10 {
					return nil, errGuestDiagnostic
				}
				out.Write(f.Data)
			case "exit":
				if f.Exit != nil && f.Exit.Code == 0 {
					return out.Bytes(), nil
				}
				return nil, errGuestDiagnostic
			case "error":
				return nil, errGuestDiagnostic
			}
		}
	}
	cmd := fixtureTartCommand(ctx, "ip", v.Name, "--wait", "5")
	data, e := cmd.Output()
	ip := strings.TrimSpace(string(data))
	if e != nil || net.ParseIP(ip) == nil {
		return nil, errGuestDiagnostic
	}
	address := net.JoinHostPort(ip, "22")
	conn, e := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", address)
	if e != nil {
		return nil, errGuestDiagnostic
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	cc, ch, req, e := ssh.NewClientConn(conn, address, &ssh.ClientConfig{User: v.Username, Auth: []ssh.AuthMethod{ssh.Password(v.Password)}, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	if e != nil {
		return nil, errGuestDiagnostic
	}
	client := ssh.NewClient(cc, ch, req)
	defer client.Close()
	session, e := client.NewSession()
	if e != nil {
		return nil, errGuestDiagnostic
	}
	defer session.Close()
	var out diagnosticBuffer
	session.Stdout = &out
	session.Stderr = io.Discard
	if session.Run(command) != nil {
		return nil, errGuestDiagnostic
	}
	return out.Bytes(), nil
}

type diagnosticBuffer struct{ bytes.Buffer }

func (b *diagnosticBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 256<<10 {
		return 0, errGuestDiagnostic
	}
	return b.Buffer.Write(p)
}

type liveReport struct {
	dir     string
	secrets []string
	mu      sync.Mutex
	extra   map[string]string
}

func newLiveReport(t *testing.T, password string) *liveReport {
	t.Helper()
	r := &liveReport{dir: os.Getenv("PULSE_TEST_OUTPUT_DIR"), secrets: []string{password, "guest-only-synthetic-value"}, extra: map[string]string{}}
	if r.dir != "" {
		if !filepath.IsAbs(r.dir) {
			t.Fatal("PULSE_TEST_OUTPUT_DIR must be absolute")
		}
		if e := os.MkdirAll(r.dir, 0700); e != nil {
			t.Fatal(e)
		}
	}
	return r
}
func (r *liveReport) addLease(l api.Lease) {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw := string(l)
	r.secrets = append(r.secrets, raw)
	var fields map[string]any
	if json.Unmarshal([]byte(raw), &fields) == nil {
		for _, key := range []string{"leaseToken", "token"} {
			if value, ok := fields[key].(string); ok {
				r.secrets = append(r.secrets, value)
			}
		}
	}
}
func redactLive(text string, secrets []string) string {
	for _, s := range secrets {
		if s == "" {
			continue
		}
		encoded, _ := json.Marshal(s)
		for _, v := range []string{s, string(encoded[1 : len(encoded)-1]), base64.StdEncoding.EncodeToString([]byte(s)), base64.RawURLEncoding.EncodeToString([]byte(s))} {
			text = strings.ReplaceAll(text, v, "[REDACTED]")
		}
	}
	return text
}
func (r *liveReport) write(t *testing.T, name string, value any) {
	t.Helper()
	if r.dir == "" {
		return
	}
	raw, e := json.Marshal(value)
	if e != nil {
		t.Error("cannot encode diagnostic report")
		return
	}
	var tree any
	if json.Unmarshal(raw, &tree) != nil {
		t.Error("cannot encode diagnostic report")
		return
	}
	r.mu.Lock()
	var clean func(any) any
	clean = func(v any) any {
		switch v := v.(type) {
		case string:
			return redactLive(v, r.secrets)
		case []any:
			for i := range v {
				v[i] = clean(v[i])
			}
			return v
		case map[string]any:
			for k, item := range v {
				v[k] = clean(item)
			}
			return v
		default:
			return v
		}
	}
	tree = clean(tree)
	r.mu.Unlock()
	data, e := json.MarshalIndent(tree, "", "  ")
	if e != nil {
		t.Error("cannot encode diagnostic report")
		return
	}
	if e = os.WriteFile(filepath.Join(r.dir, name), append(data, '\n'), 0600); e != nil {
		t.Error("cannot save diagnostic report")
	}
}
func (r *liveReport) capture(t *testing.T, ctx context.Context, kind string, connection OrchardConfig, runtime *Runtime, stateDir string) {
	t.Helper()
	if r.dir == "" || runtime == nil {
		return
	}
	if diagnostic, ok := runtime.p.Backend.(interface {
		EndpointDiagnostics(context.Context) (map[string]map[string]string, error)
	}); ok {
		data, err := diagnostic.EndpointDiagnostics(ctx)
		if err != nil {
			t.Logf("endpoint diagnostics: %v", err)
		}
		r.write(t, fmt.Sprintf("endpoint-diagnostics-%d.json", time.Now().UnixNano()), data)
	}
	var journal struct {
		Records map[string]struct{ Name, UID string } `json:"records"`
		Slots   map[string]struct{ Name, UID string } `json:"slots"`
		Images  map[string]struct{ Name, UID string } `json:"images"`
		Pairs   map[string]struct {
			Primary, Engine       struct{ Name string }
			PrimaryUID, EngineUID string
		} `json:"pairs"`
	}
	data, _ := os.ReadFile(filepath.Join(stateDir, "state.json"))
	_ = json.Unmarshal(data, &journal)
	owned := map[string]string{}
	for _, v := range journal.Records {
		owned[v.Name] = v.UID
	}
	for _, v := range journal.Slots {
		owned[v.Name] = v.UID
	}
	for _, v := range journal.Images {
		owned[v.Name] = v.UID
	}
	for _, pair := range journal.Pairs {
		if pair.PrimaryUID != "" {
			owned[pair.Primary.Name] = pair.PrimaryUID
		}
		if pair.EngineUID != "" {
			owned[pair.Engine.Name] = pair.EngineUID
		}
	}
	for n, uid := range r.extra {
		owned[n] = uid
	}
	type diagnostic struct {
		Name             string `json:"name"`
		UID              string `json:"uid"`
		Status           string `json:"status"`
		Power            string `json:"power"`
		Events           string `json:"events,omitempty"`
		RunLog           string `json:"tart_run_log,omitempty"`
		GuestLogs        string `json:"guest_logs,omitempty"`
		ObservationError string `json:"observation_error,omitempty"`
	}
	output := []diagnostic{}
	for name, uid := range owned {
		d := diagnostic{Name: name, UID: uid}
		v, e := runtime.p.Backend.Get(ctx, name)
		if backend.IsStatus(e, 404) {
			d.Status = "removed"
			output = append(output, d)
			continue
		}
		if e != nil || v.UID != uid {
			d.ObservationError = "native observation unavailable or ownership mismatch"
			output = append(output, d)
			continue
		}
		if kind == "tart" && config.Name(name) {
			f, e := os.Open(filepath.Join(stateDir, "native", name+".run.log"))
			if e == nil {
				if st, e := f.Stat(); e == nil && st.Size() > 64000 {
					_, _ = f.Seek(-64000, io.SeekEnd)
				}
				data, _ := io.ReadAll(io.LimitReader(f, 64000))
				f.Close()
				d.RunLog = string(data)
			}
		}
		d.Status = v.Status
		d.Power = v.PowerState
		if events, e := runtime.p.Backend.(interface {
			Events(context.Context, string) ([]backend.Event, error)
		}).Events(ctx, name); e == nil {
			for _, ev := range events {
				d.Events += ev.Payload + "\n" + ev.Message + "\n"
			}
		}
		if !v.Stopped() {
			logs, e := testGuestExec(ctx, kind, connection, v, `for f in /var/log/pulse-tart-build.log /var/log/pulse-tart-base.log /var/log/pulse-tart-docker.log /var/tmp/pulse-docker-contract.log /var/log/pulse-tart-template.log /var/log/pulse-tart-bootstrap.log /var/log/pulse-tart-guest.log /var/log/pulse-fixture.log /var/db/pulse-tart/worker.log; do if sudo -n test -f "$f"; then printf '\n%s\n' "$f"; sudo -n tail -c 16000 "$f"; fi; done`)
			if e == nil {
				d.GuestLogs = string(logs)
			} else {
				d.ObservationError = "guest logs unavailable"
			}
		}
		output = append(output, d)
	}
	r.write(t, fmt.Sprintf("vm-diagnostics-%d.json", time.Now().UnixNano()), output)
}

const fixtureStartup = `#!/bin/bash
set -eu
set +x
sudo -n mkdir -p /var/db/pulse-fixture
sudo -n tee /Library/LaunchDaemons/dev.pulse-tart.fixture.plist >/dev/null <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>Label</key><string>dev.pulse-tart.fixture</string>
<key>ProgramArguments</key><array><string>/opt/pulse-tart/bin/pulse-orchard-fixture</string></array>
<key>EnvironmentVariables</key><dict><key>HOME</key><string>/var/root</string><key>PATH</key><string>/opt/pulse-tart/bin:/nix/var/nix/profiles/default/bin:/usr/bin:/bin:/usr/sbin:/sbin</string></dict>
<key>RunAtLoad</key><true/><key>StandardOutPath</key><string>/var/log/pulse-fixture.log</string><key>StandardErrorPath</key><string>/var/log/pulse-fixture.log</string>
</dict></plist>
PLIST
sudo -n chmod 600 /Library/LaunchDaemons/dev.pulse-tart.fixture.plist
sudo -n launchctl print system/dev.pulse-tart.fixture >/dev/null 2>&1 || sudo -n launchctl bootstrap system /Library/LaunchDaemons/dev.pulse-tart.fixture.plist
for ((i=0;i<120;i++)); do
 if /usr/bin/curl --silent --fail http://127.0.0.1:18881/health >/dev/null; then echo PULSE_JOBDB_FIXTURE_READY; exit 0; fi
 sleep 1
done
exit 1
`

type fixtureStatus struct {
	Status      string `json:"status"`
	OutputOK    bool   `json:"output_ok"`
	Claims      int    `json:"claims"`
	Renewals    int    `json:"renewals"`
	SecretCalls int    `json:"secret_calls"`
}

func exerciseJobDB(t *testing.T, ctx context.Context, kind string, connection OrchardConfig, r *Runtime, owner, stateDir string, report *liveReport, done func([]api.Launch)) {
	t.Helper()
	slots, e := r.Slots()
	if e != nil || len(slots) != 2 {
		t.Fatal("fixture needs two prepared slots")
	}
	// The coordinator occupies the second slot's MAC; public Submit consequently
	// uses the first slot. Only two guests run at once, including the JobDB service.
	source, e := r.p.Backend.Get(ctx, slots[1].Template)
	if e != nil {
		t.Fatal(e)
	}
	spec := source.Spec
	spec.Name = "pulse-fixture-" + owner
	spec.Image = source.LocalName
	spec.StartupScript = backend.Script{Content: fixtureStartup, Env: map[string]string{}}
	intent := fixtureSpecBytes(spec)
	if e = os.WriteFile(filepath.Join(stateDir, "fixture-intent.json"), intent, 0600); e != nil {
		t.Fatal("cannot persist fixture intent")
	}
	coordinator := backend.VM{Spec: spec}
	report.extra[spec.Name] = ""
	defer func() {
		clean, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		report.capture(t, clean, kind, connection, r, stateDir)
		for clean.Err() == nil {
			v, e := r.p.Backend.Get(clean, coordinator.Name)
			if backend.IsStatus(e, 404) {
				return
			}
			actual := fixtureSpecBytes(v.Spec)
			if e != nil || (coordinator.UID != "" && v.UID != coordinator.UID) || !bytes.Equal(actual, intent) {
				t.Error("fixture cleanup ownership check failed")
				return
			}
			if !v.Stopped() {
				_ = r.p.Backend.(interface {
					Suspend(context.Context, string, string) error
				}).Suspend(clean, v.Name, v.UID)
			} else {
				_ = r.p.Backend.Delete(clean, v.Name)
			}
			time.Sleep(time.Second)
		}
		t.Error("fixture coordinator cleanup incomplete")
	}()
	t.Log("creating JobDB fixture VM")
	created, createErr := r.p.Backend.Create(ctx, spec)
	if createErr != nil {
		t.Fatal("fixture coordinator create uncertain; preserved intent for recovery")
	}
	coordinator = created
	report.extra[coordinator.Name] = coordinator.UID
	waitLive(t, ctx, "JobDB service", 3*time.Minute, func(ctx context.Context) bool {
		data, e := testGuestExec(ctx, kind, connection, coordinator, "/usr/bin/curl --silent --fail --max-time 10 http://127.0.0.1:18881/health")
		return e == nil && bytes.Contains(data, []byte(`"ready":true`))
	})
	fetch := func(path string, out any) {
		t.Helper()
		data, e := testGuestExec(ctx, kind, connection, coordinator, "/usr/bin/curl --silent --fail --max-time 15 'http://127.0.0.1:18881/"+path+"'")
		if e != nil || json.Unmarshal(data, out) != nil {
			t.Fatal("fixture control request failed")
		}
	}
	assignment := func(name, id string) api.Launch {
		t.Helper()
		var a api.Launch
		fetch("launch?case="+name, &a)
		var wire struct {
			Version int `json:"version"`
		}
		if json.Unmarshal([]byte(a.Lease), &wire) != nil || wire.Version != 2 {
			t.Fatal("fixture must export the current JobDB v2 lease format")
		}
		a.LaunchID = owner + "-" + id
		a.Allocation = api.Allocation{Base: "macos*", Capabilities: []string{"nix", "git"}, CPUMillis: 1000, MemoryBytes: 2 << 30}
		report.addLease(a.Lease)
		return a
	}
	submit := func(l api.Launch) {
		t.Helper()
		out, e := r.Submit(ctx, []api.Launch{l})
		if e != nil || len(out) != 1 || out[0].Status != api.Accepted {
			t.Fatal("JobDB launch was not accepted", out, e)
		}
		page, e := r.List(ctx, api.ListRequest{LaunchID: l.LaunchID})
		if e != nil || len(page.Items) != 1 || page.Items[0].Job != l.Job {
			t.Fatal("JobDB correlation was not preserved")
		}
		worker, e := r.p.Backend.Get(ctx, page.Items[0].ID)
		if e != nil {
			t.Fatal(e)
		}
		t.Logf("connecting JobDB SSH tunnel for %s", l.LaunchID)
		tunnel, err := attachFixtureTunnel(ctx, kind, connection, coordinator, worker)
		if err != nil {
			t.Fatal("fixture SSH tunnel unavailable")
		}
		defer tunnel()
		done([]api.Launch{l})
		if e = r.Reconcile(ctx); e != nil {
			t.Fatal(e)
		}
	}
	valid := assignment("valid", "jobdb-valid")
	submit(valid)
	var completed fixtureStatus
	fetch("status?case=valid", &completed)
	report.write(t, "jobdb-valid.json", completed)
	if completed.Status != "COMPLETED" || !completed.OutputOK || completed.Claims != 1 || completed.Renewals < 2 || completed.SecretCalls < 1 {
		t.Fatalf("JobDB execution contract failed: status=%s claims=%d renewals=%d secret_calls=%d", completed.Status, completed.Claims, completed.Renewals, completed.SecretCalls)
	}
	t.Logf("JOBDB_VALID_PASS: completed; macos-*arm64 requirement; lease v2; Nix hello; synthetic secret retrieval; claims=%d renewals=%d secret_calls=%d", completed.Claims, completed.Renewals, completed.SecretCalls)
	replay := valid
	replay.LaunchID = owner + "-jobdb-replay"
	submit(replay)
	var after fixtureStatus
	fetch("status?case=valid", &after)
	report.write(t, "jobdb-replay.json", after)
	if after.Claims != 1 || after.Status != "COMPLETED" || after.SecretCalls != completed.SecretCalls {
		t.Fatal("completed lease was reused or replacement work claimed")
	}
	t.Log("JOBDB_REPLAY_PASS: completed lease rejected without reacquisition")
	expired := assignment("expired", "jobdb-expired")
	submit(expired)
	var expiration fixtureStatus
	fetch("status?case=expired", &expiration)
	report.write(t, "jobdb-expired.json", expiration)
	if expiration.Claims != 2 || expiration.Status == "COMPLETED" || expiration.OutputOK || expiration.SecretCalls != completed.SecretCalls {
		t.Fatal("expired lease executed or reacquired work")
	}
	t.Log("JOBDB_EXPIRED_PASS: expired lease rejected without execution or reacquisition")
}

func fixtureSpecBytes(spec backend.Spec) []byte {
	if spec.StartupScript.Env == nil {
		spec.StartupScript.Env = map[string]string{}
	}
	if spec.Labels == nil {
		spec.Labels = map[string]string{}
	}
	data, _ := json.Marshal(spec)
	return data
}

// Both sides connect outward from the test host via SSH. JobDB and Git remain
// on guest loopback addresses; no guest-to-guest route or exposed host port is needed.
func openFixtureSSH(ctx context.Context, kind string, connection OrchardConfig, v backend.VM) (*ssh.Client, error) {
	var conn net.Conn
	var e error
	if kind == "orchard" {
		base, err := config.NormalizeURL(connection.URL, connection.AllowInsecureHTTP)
		if err != nil {
			return nil, errGuestDiagnostic
		}
		u, _ := url.Parse(base + "/vms/" + url.PathEscape(v.Name) + "/port-forward")
		origin := u.Scheme + "://" + u.Host
		if u.Scheme == "https" {
			u.Scheme = "wss"
		} else {
			u.Scheme = "ws"
		}
		u.RawQuery = url.Values{"port": {"22"}, "wait": {"5"}}.Encode()
		cfg, err := websocket.NewConfig(u.String(), origin)
		if err != nil {
			return nil, errGuestDiagnostic
		}
		cfg.Dialer = &net.Dialer{Timeout: 10 * time.Second}
		cfg.Header.Set("User-Agent", "pulse-cell-integration")
		if connection.Token != "" {
			req, _ := http.NewRequest("GET", origin, nil)
			req.SetBasicAuth(connection.Username, string(connection.Token))
			cfg.Header.Set("Authorization", req.Header.Get("Authorization"))
		}
		ws, err := websocket.DialConfig(cfg)
		if err != nil {
			return nil, errGuestDiagnostic
		}
		ws.PayloadType = websocket.BinaryFrame
		conn = ws
	} else {
		command := fixtureTartCommand(ctx, "ip", v.Name, "--wait", "5")
		data, err := command.Output()
		ip := strings.TrimSpace(string(data))
		if err != nil || net.ParseIP(ip) == nil {
			return nil, errGuestDiagnostic
		}
		conn, e = (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(ip, "22"))
		if e != nil {
			return nil, errGuestDiagnostic
		}
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	cc, ch, req, e := ssh.NewClientConn(conn, v.Name, &ssh.ClientConfig{User: v.Username, Auth: []ssh.AuthMethod{ssh.Password(v.Password)}, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	if e != nil {
		stop()
		conn.Close()
		return nil, errGuestDiagnostic
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(cc, ch, req), nil
}
func attachFixtureTunnel(ctx context.Context, kind string, connection OrchardConfig, coordinator, worker backend.VM) (func(), error) {
	ctx, cancel := context.WithCancel(ctx)
	var workerSSH *ssh.Client
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		c, e := openFixtureSSH(ctx, kind, connection, worker)
		if e == nil {
			workerSSH = c
			break
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	if workerSSH == nil {
		cancel()
		return nil, errGuestDiagnostic
	}
	coordinatorSSH, e := openFixtureSSH(ctx, kind, connection, coordinator)
	if e != nil {
		cancel()
		workerSSH.Close()
		return nil, errGuestDiagnostic
	}
	listener, e := workerSSH.Listen("tcp", "127.0.0.1:18880")
	if e != nil {
		cancel()
		workerSSH.Close()
		coordinatorSSH.Close()
		return nil, errGuestDiagnostic
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			in, e := listener.Accept()
			if e != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer in.Close()
				out, e := coordinatorSSH.Dial("tcp", "127.0.0.1:18880")
				if e != nil {
					return
				}
				defer out.Close()
				copied := make(chan struct{})
				go func() { _, _ = io.Copy(out, in); _ = out.Close(); close(copied) }()
				_, _ = io.Copy(in, out)
				_ = in.Close()
				<-copied
			}()
		}
	}()
	return func() { cancel(); listener.Close(); workerSSH.Close(); coordinatorSSH.Close(); wg.Wait() }, nil
}

func fixtureTartCommand(ctx context.Context, args ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, "tart", args...)
	home := os.Getenv("PULSE_TEST_TART_HOME")
	if home == "" {
		userHome, _ := os.UserHomeDir()
		home = filepath.Join(userHome, ".tart")
	}
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "TART_HOME=") {
			command.Env = append(command.Env, value)
		}
	}
	command.Env = append(command.Env, "TART_HOME="+home)
	return command
}
