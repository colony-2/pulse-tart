package tart

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-tart/internal/backend"
	native "github.com/colony-2/pulse-tart/internal/tart"
)

// Preserve every native backend operation, guarding dispatch itself (including
// base/template builders), rather than merely sampling the running VM count.
type qualificationBackend struct {
	*native.Client
	limitError error
}

func (b *qualificationBackend) Create(ctx context.Context, spec backend.Spec) (backend.VM, error) {
	if spec.OS != "linux" {
		vms, err := b.List(ctx)
		if err != nil {
			return backend.VM{}, err
		}
		running := 0
		for _, v := range vms {
			if v.OS != "linux" && !v.Stopped() {
				running++
			}
		}
		if running >= 2 {
			b.limitError = fmt.Errorf("qualification refuses a third running macOS VM")
			return backend.VM{}, b.limitError
		}
	}
	return b.Client.Create(ctx, spec)
}

type qualificationOps interface {
	Exec(context.Context, string, string, []byte) ([]byte, error)
	Stop(context.Context, string, string) error
}
type qualification struct {
	t          *testing.T
	ctx        context.Context
	cfg        Config
	options    TartConfig
	r          *Runtime
	report     *liveReport
	checks     map[string]bool
	identities []map[string]string
	peakMac    int
}

func (q *qualification) open() {
	q.t.Helper()
	r, err := NewTart(q.cfg, q.options)
	if err != nil {
		q.t.Fatal(err)
	}
	r.profile = "docker-integration"
	r.p.Backend = &qualificationBackend{Client: r.p.Backend.(*native.Client)}
	q.r = r
}
func (q *qualification) reopen(count int) {
	q.t.Helper()
	if err := q.r.Close(); err != nil {
		q.t.Fatal(err)
	}
	q.cfg.Slots[0].Count = count
	q.open()
}
func (q *qualification) ops() qualificationOps { return q.r.p.Backend.(qualificationOps) }
func (q *qualification) execContext(ctx context.Context, p DockerStatus, engine bool, script string) ([]byte, error) {
	name, uid := p.Primary, p.PrimaryUID
	if engine {
		name, uid = p.Engine, p.EngineUID
	}
	// Exec also verifies the UID, including after a controller reopen.
	return q.ops().Exec(ctx, name, uid, []byte("set -eu\n"+script+"\n"))
}
func (q *qualification) run(p DockerStatus, engine bool, script string) string {
	q.t.Helper()
	ctx, cancel := context.WithTimeout(q.ctx, 3*time.Minute)
	defer cancel()
	out, err := q.execContext(ctx, p, engine, script)
	if err != nil {
		q.t.Fatalf("guest command failed (%s engine=%v): %v\n%s", p.ID, engine, err, out)
	}
	return strings.TrimSpace(string(out))
}
func (q *qualification) checkpoint(name string) {
	q.t.Helper()
	q.checks[name] = true
	q.report.write(q.t, "qualification.json", map[string]any{"checks": q.checks, "identities": q.identities, "peak_running_macos": q.peakMac})
	q.t.Log("qualification passed:", name)
}
func (q *qualification) reconcile(ctx context.Context) {
	q.t.Helper()
	if err := q.r.Reconcile(ctx); err != nil {
		q.t.Log("reconcile:", err)
	}
	if err := q.r.p.Backend.(*qualificationBackend).limitError; err != nil {
		q.t.Fatal(err)
	}
	vms, err := q.r.p.Backend.List(ctx)
	if err != nil {
		q.t.Fatal("inventory", err)
	}
	mac := 0
	for _, v := range vms {
		if v.OS != "linux" && !v.Stopped() {
			mac++
		}
	}
	if mac > q.peakMac {
		q.peakMac = mac
	}
	if mac > 2 {
		q.t.Fatal("macOS running limit exceeded", mac)
	}
}
func (q *qualification) ready() []DockerStatus {
	q.t.Helper()
	var pairs []DockerStatus
	waitLive(q.t, q.ctx, "two production Docker allocations", 35*time.Minute, func(ctx context.Context) bool {
		q.reconcile(ctx)
		images, err := q.r.Images()
		if err != nil {
			q.t.Fatal(err)
		}
		for _, image := range images {
			if image.Phase == "failed" || image.Phase == "quarantine" {
				q.t.Fatal("image preparation failed", image.Name, image.Phase)
			}
		}
		slots, err := q.r.Slots()
		if err != nil {
			q.t.Fatal(err)
		}
		for _, slot := range slots {
			if slot.Phase == "failed" || slot.Phase == "quarantine" {
				q.t.Fatal("template preparation failed", slot.ID, slot.Phase)
			}
		}
		pairs, err = q.r.DockerAllocations()
		if err != nil {
			q.t.Fatal(err)
		}
		for _, p := range pairs {
			if p.Phase == "quarantine" || p.Phase == "reclaiming" {
				q.t.Fatal("allocation failed during preparation", p.ID, p.Phase)
			}
		}
		return len(pairs) == 2 && pairs[0].Phase == "ready" && pairs[1].Phase == "ready"
	})
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].Slot < pairs[j].Slot })
	for _, p := range pairs {
		facts := strings.Fields(q.run(p, false, `sudo -n /opt/pulse-tart/bin/python3 - <<'PY'
import hashlib,json,pathlib,subprocess
print(subprocess.check_output(['/opt/pulse-tart/bin/docker','info','--format','{{.ID}}'],text=True).strip())
for name in ('client-key','host-key.pub'):
 print(hashlib.sha256((pathlib.Path('/var/lib/pulse-docker')/name).read_bytes()).hexdigest())
PY`))
		if len(facts) != 3 {
			q.t.Fatal("invalid identity inventory")
		}
		for _, old := range q.identities {
			if old["engine_id"] == facts[0] || old["client_key_sha256"] == facts[1] || old["host_key_sha256"] == facts[2] || old["primary_uid"] == p.PrimaryUID {
				q.t.Fatal("allocation identity reused")
			}
		}
		q.identities = append(q.identities, map[string]string{"id": p.ID, "primary_uid": p.PrimaryUID, "engine_id": facts[0], "client_key_sha256": facts[1], "host_key_sha256": facts[2]})
	}
	// Observe live clone MACs: published template status may omit old observations.
	macs := map[string]bool{}
	for i, p := range pairs {
		mac := q.run(p, false, "/sbin/ifconfig en0 | /usr/bin/awk '/ether / {print $2}'")
		if mac == "" || macs[mac] {
			q.t.Fatal("two distinct primary MACs required")
		}
		macs[mac] = true
		q.identities[len(q.identities)-len(pairs)+i]["mac"] = mac
	}

	return pairs
}
func qualificationUpload(path string, data []byte) string {
	return fmt.Sprintf("printf '%%s' '%s' | base64 -D | sudo -n tee %s >/dev/null\nsudo -n chmod 755 %s\n", base64.StdEncoding.EncodeToString(data), path, path)
}
func (q *qualification) startJobs(pairs []DockerStatus) []api.Launch {
	q.t.Helper()
	contract, err := os.ReadFile("scripts/docker-qualification/job-contract.sh")
	if err != nil {
		q.t.Fatal(err)
	}
	launches := []api.Launch{}
	for _, p := range pairs {
		q.run(p, false, "sudo -n mkdir -p /opt/pulse-docker\n"+qualificationUpload("/opt/pulse-docker/engine-contract.sh", contract)+fmt.Sprintf("printf '%%s' '%s' | sudo -n tee /opt/pulse-docker/qualification-marker >/dev/null\nsudo -n chmod 644 /opt/pulse-docker/qualification-marker\n", p.ID)+strings.ReplaceAll(fixtureStartup, "pulse-orchard-fixture", "pulse-docker-fixture"))
		waitLive(q.t, q.ctx, "qualification JobDB ready", time.Minute, func(ctx context.Context) bool {
			_, err := q.execContext(ctx, p, false, "curl --max-time 5 -fsS http://127.0.0.1:18881/health")
			return err == nil
		})
		var launch api.Launch
		if err := json.Unmarshal([]byte(q.run(p, false, "curl -fsS 'http://127.0.0.1:18881/launch?case=valid'")), &launch); err != nil {
			q.t.Fatal("lease decode", err)
		}
		q.report.addLease(launch.Lease)
		launch.LaunchID = "qualification-" + p.ID
		launch.Allocation = api.Allocation{Base: q.cfg.Bases[0].Name, Capabilities: []string{"docker", "git"}, CPUMillis: 1000, MemoryBytes: 2 << 30}
		out, err := q.r.Submit(q.ctx, []api.Launch{launch})
		if err != nil || len(out) != 1 || out[0].Status != api.Accepted {
			q.t.Fatal("qualification admission failed", err)
		}
		page, err := q.r.List(q.ctx, api.ListRequest{LaunchID: launch.LaunchID})
		if err != nil || len(page.Items) != 2 {
			q.t.Fatal("allocation resource accounting", err)
		}
		launches = append(launches, launch)
	}
	// Leases refer to the JobDB on their own primary: verify deterministic placement.
	assigned, err := q.r.DockerAllocations()
	if err != nil {
		q.t.Fatal(err)
	}
	for _, p := range assigned {
		if p.LaunchID != "qualification-"+p.ID {
			q.t.Fatal("unexpected lease placement")
		}
	}
	for _, p := range pairs {
		waitLive(q.t, q.ctx, "c2j Docker job started", 4*time.Minute, func(ctx context.Context) bool {
			out, err := q.execContext(ctx, p, false, "cat /var/tmp/qualification-started")
			return err == nil && strings.TrimSpace(string(out)) == p.ID
		})
	}
	return launches
}

const qualificationWorker = "sudo -n -u pulseworker env HOME=/Users/pulseworker PATH=/opt/pulse-tart/bin:/usr/bin:/bin:/usr/sbin:/sbin DOCKER_HOST=unix:///var/run/docker.sock /bin/bash -s <<'QUALIFICATION_WORKER'\n"

func (q *qualification) worker(p DockerStatus, script string) string {
	return q.run(p, false, qualificationWorker+script+"\nQUALIFICATION_WORKER")
}
func (q *qualification) healthy(p DockerStatus) {
	q.t.Helper()
	waitLive(q.t, q.ctx, "Docker service recovery", 90*time.Second, func(ctx context.Context) bool {
		out, err := q.execContext(ctx, p, false, "curl --max-time 3 -fsS http://127.0.0.1:41081/marker")
		return err == nil && strings.TrimSpace(string(out)) == p.ID
	})
}
func (q *qualification) status(p DockerStatus) fixtureStatus {
	q.t.Helper()
	var s fixtureStatus
	if err := json.Unmarshal([]byte(q.run(p, false, "curl -fsS 'http://127.0.0.1:18881/status?case=valid'")), &s); err != nil {
		q.t.Fatal(err)
	}
	return s
}
func (q *qualification) overlap(pairs []DockerStatus) {
	q.t.Helper()
	start := time.Now()
	waitLive(q.t, q.ctx, "overlapping real c2j jobs and renewals", 2*time.Minute, func(ctx context.Context) bool {
		for _, p := range pairs {
			q.healthy(p)
			s := q.status(p)
			if s.Status == "FAILED" || s.Status == "COMPLETED" || s.Claims != 1 {
				q.t.Fatal("job failed or completed before release", s.Status, s.Claims)
			}
			if s.Renewals < 2 {
				return false
			}
		}
		return time.Since(start) >= 30*time.Second
	})
	q.report.write(q.t, "overlap.json", map[string]any{"seconds": time.Since(start).Seconds(), "jobs": 2})
	q.checkpoint("concurrent_jobs")
}

// This helper owns the actual Runtime in a separate process. SIGKILL exercises
// journal/file-lock recovery, not just Runtime.Close in the test process.
func TestDockerQualificationController(t *testing.T) {
	path := os.Getenv("PULSE_QUALIFICATION_CONTROLLER")
	if path == "" {
		t.Skip("qualification subprocess only")
	}
	var input struct {
		Config  Config
		Options TartConfig
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	r, err := NewTart(input.Config, input.Options)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.profile = "docker-integration"
	r.p.Backend = &qualificationBackend{Client: r.p.Backend.(*native.Client)}
	if err = r.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path+".ready", []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	// Intentionally no deferred journal close is reached after SIGKILL.
	<-t.Context().Done()
}
func (q *qualification) controllerCrash(launches []api.Launch) {
	q.t.Helper()
	path := filepath.Join(q.cfg.StateDir, "controller-input.json")
	input, _ := json.Marshal(struct {
		Config  Config
		Options TartConfig
	}{q.cfg, q.options})
	if err := os.WriteFile(path, input, 0600); err != nil {
		q.t.Fatal(err)
	}
	defer os.Remove(path)
	defer os.Remove(path + ".ready")
	if err := q.r.Close(); err != nil {
		q.t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDockerQualificationController$", "-test.timeout=3m")
	cmd.Env = append(os.Environ(), "PULSE_QUALIFICATION_CONTROLLER="+path)
	log, err := os.OpenFile(filepath.Join(q.cfg.StateDir, "controller.log"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		q.t.Fatal(err)
	}
	defer log.Close()
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		q.open()
		q.t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		data, _ := os.ReadFile(log.Name())
		q.report.write(q.t, "controller-crash.json", map[string]any{"log": string(data), "process_state": cmd.ProcessState.String()})
		if q.r.closed {
			q.open()
		}
	}()
	waitLive(q.t, q.ctx, "separate controller owns journal", time.Minute, func(context.Context) bool { _, err := os.Stat(path + ".ready"); return err == nil })
	if err := cmd.Process.Kill(); err != nil {
		q.t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		q.t.Fatal("controller did not die abruptly")
	}
	q.open()
	for _, launch := range launches {
		out, err := q.r.Submit(q.ctx, []api.Launch{launch})
		if err != nil || len(out) != 1 || out[0].Status != api.Accepted {
			q.t.Fatal("crash replay failed", err)
		}
	}
	q.checkpoint("controller_sigkill_replay")
}
func (q *qualification) pressure(p, peer DockerStatus, engine bool, script, name string) {
	q.t.Helper()
	ctx, cancel := context.WithTimeout(q.ctx, 4*time.Minute)
	defer cancel()
	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	go func() { out, err := q.execContext(ctx, p, engine, script); done <- result{out, err} }()
	samples := 0
	for {
		select {
		case r := <-done:
			q.report.write(q.t, name+".json", map[string]any{"output": string(r.out), "peer_successful_probes": samples})
			if r.err != nil {
				q.t.Fatalf("%s failed: %v\n%s", name, r.err, r.out)
			}
			if samples < 3 {
				q.t.Fatal("pressure did not overlap sufficient peer probes", samples)
			}
			q.healthy(p)
			q.checkpoint(name)
			return
		case <-ctx.Done():
			q.t.Fatal("pressure deadline", name)
		case <-time.After(2 * time.Second):
			probe, cancel := context.WithTimeout(ctx, 8*time.Second)
			out, err := q.execContext(probe, peer, false, "curl --max-time 3 -fsS http://127.0.0.1:41081/marker")
			cancel()
			if err != nil || strings.TrimSpace(string(out)) != peer.ID {
				q.t.Fatal("peer disrupted by pressure", name, err)
			}
			samples++
		}
	}
}
func (q *qualification) complete(pairs []DockerStatus) {
	q.t.Helper()
	for _, p := range pairs {
		// The guest shuts down after c2j exits. Release/capture one at a time.
		q.run(p, false, "sudo -n touch /var/tmp/qualification-release")
		var s fixtureStatus
		waitLive(q.t, q.ctx, "c2j qualification completion", 3*time.Minute, func(context.Context) bool {
			s = q.status(p)
			if s.Status == "FAILED" {
				q.t.Fatal("qualification job failed")
			}
			return s.Status == "COMPLETED"
		})
		q.report.write(q.t, "jobdb-"+p.ID+".json", s)
		if !s.OutputOK || s.Claims != 1 || s.Renewals < 2 || s.SecretCalls != 1 {
			q.t.Fatal("invalid real JobDB evidence")
		}
	}
	q.reopen(0)
	waitLive(q.t, q.ctx, "paired cleanup after completion", 3*time.Minute, func(ctx context.Context) bool {
		q.reconcile(ctx)
		pairs, _ := q.r.DockerAllocations()
		return len(pairs) == 0
	})
	q.assertGone(pairs)
}
func (q *qualification) assertGone(pairs []DockerStatus) {
	q.t.Helper()
	for _, p := range pairs {
		for _, name := range []string{p.Primary, p.Engine} {
			if _, err := q.r.p.Backend.Get(q.ctx, name); !backend.IsStatus(err, 404) {
				q.t.Fatal("VM not removed", name, err)
			}
		}
	}
	// Native helper must be booted out and its allocation directory removed.
	for _, p := range pairs {
		if _, err := os.Stat(filepath.Join(q.cfg.StateDir, "native", "endpoints", p.EngineUID)); !os.IsNotExist(err) {
			q.t.Fatal("endpoint not removed", p.ID, err)
		}
	}
}
func (q *qualification) cleanup() {
	q.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	q.report.capture(q.t, ctx, "tart", OrchardConfig{}, q.r, q.cfg.StateDir)
	// Only this test's UID-verified disposable allocations may be stopped on failure.
	pairs, _ := q.r.DockerAllocations()
	for _, p := range pairs {
		for _, v := range [][2]string{{p.Primary, p.PrimaryUID}, {p.Engine, p.EngineUID}} {
			if v[1] != "" {
				_ = q.ops().Stop(ctx, v[0], v[1])
			}
		}
	}
	_ = q.r.Drain(ctx)
	images, _ := q.r.Images()
	for _, im := range images {
		v, err := q.r.p.Backend.Get(ctx, im.Name)
		if err == nil && v.UID == im.UID && !v.Stopped() {
			_ = q.ops().Stop(ctx, v.Name, v.UID)
		}
	}
	for ctx.Err() == nil {
		_ = q.r.Reconcile(ctx)
		pairs, err := q.r.DockerAllocations()
		slots, e := q.r.Slots()
		if err == nil && e == nil && len(pairs) == 0 && len(slots) == 0 {
			if err := q.r.PurgeImages(ctx); err != nil {
				q.t.Error("purge", err)
			}
			q.r.Close()
			if !q.t.Failed() {
				os.RemoveAll(q.cfg.StateDir)
			}
			return
		}
		time.Sleep(time.Second)
	}
	q.t.Errorf("cleanup incomplete; private recovery state retained at %s", q.cfg.StateDir)
	q.r.Close()
}

func TestLiveDockerQualification(t *testing.T) {
	if os.Getenv("PULSE_TEST_DOCKER_QUALIFICATION") != "1" {
		t.Skip("use scripts/test-macos-docker-qualification.sh")
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Fatal("native qualification requires Apple Silicon macOS")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Minute)
	defer cancel()
	dir, err := os.MkdirTemp("", "pulse-docker-qualification-")
	if err != nil {
		t.Fatal(err)
	}
	engine := DefaultDockerEngine()
	engine.DiskGiB = 8
	base := DefaultBases()[0]
	q := &qualification{t: t, ctx: ctx, report: newLiveReport(t, string(base.SSHPassword)), checks: map[string]bool{}, cfg: Config{OwnerID: fmt.Sprintf("docker-qualification-%d", time.Now().UnixNano()), StateDir: dir, Bases: []Base{base}, C2JVersion: os.Getenv("PULSE_TEST_C2J_VERSION"), Slots: []SlotGroup{{Name: "standard", Base: base.Name, Count: 2, DockerProfile: "standard"}}, Docker: &DockerConfig{Profiles: []DockerEngineProfile{engine}}}, options: TartConfig{Home: os.Getenv("PULSE_TEST_TART_HOME"), DockerEndpoint: DockerEndpoint{Address: os.Getenv("PULSE_TEST_ENGINE_WORKER_ADDRESS"), PortMin: 45200, PortMax: 45201}}}
	q.open()
	defer q.cleanup()
	t.Log("private recovery state:", dir)
	pairs := q.ready()
	launches := q.startJobs(pairs)
	q.overlap(pairs)
	q.controllerCrash(launches)
	a, b := pairs[0], pairs[1]
	q.healthy(a)
	q.healthy(b)
	// Force the guest relay process to die. launchd must recreate it.
	before := q.run(a, false, "sudo -n launchctl list dev.pulse-tart.docker")
	q.run(a, false, "sudo -n launchctl kill SIGKILL system/dev.pulse-tart.docker")
	q.healthy(a)
	q.healthy(b)
	after := q.run(a, false, "sudo -n launchctl list dev.pulse-tart.docker")
	if before == after {
		t.Fatal("relay process was not restarted")
	}
	q.checkpoint("guest_relay_sigkill")
	// The worker endpoint service must also survive an abrupt process death.
	label := "user/" + strconv.Itoa(os.Getuid()) + "/dev.pulse-tart.engine." + a.EngineUID
	if out, err := exec.CommandContext(ctx, "/bin/launchctl", "kill", "SIGKILL", label).CombinedOutput(); err != nil {
		t.Fatalf("endpoint kill: %v %s", err, out)
	}
	q.healthy(a)
	q.healthy(b)
	q.checkpoint("worker_endpoint_sigkill")
	q.run(a, true, "sudo -n systemctl kill --kill-whom=main --signal=SIGKILL docker.service\nsudo -n systemctl restart docker.service")
	q.healthy(a)
	q.healthy(b)
	q.checkpoint("docker_daemon_sigkill")
	pressure, err := os.ReadFile("scripts/docker-qualification/pressure.sh")
	if err != nil {
		t.Fatal(err)
	}
	q.pressure(a, b, false, qualificationWorker+string(pressure)+"\nQUALIFICATION_WORKER", "cpu_memory_pressure")
	disk, err := os.ReadFile("scripts/docker-qualification/disk-pressure.py")
	if err != nil {
		t.Fatal(err)
	}
	q.pressure(a, b, true, "sudo -n python3 - <<'QUALIFICATION_DISK'\n"+string(disk)+"\nQUALIFICATION_DISK", "private_disk_enospc")
	q.worker(a, "docker system prune -af --volumes\ndocker volume prune -af\n! docker volume inspect qualification-unused >/dev/null 2>&1")
	if q.worker(b, "docker run --rm -v qualification-unused:/data python:3.13-alpine cat /data/marker") != b.ID {
		t.Fatal("prune crossed allocation boundary")
	}
	q.healthy(a)
	q.healthy(b)
	q.checkpoint("prune_isolation")
	q.complete(pairs)
	q.checkpoint("successful_pair_cleanup")
	// Both slots get new identities. Terminate a primary, then its peer's engine,
	// while c2j jobs are running, and require complete paired reclamation.
	q.reopen(2)
	pairs = q.ready()
	q.startJobs(pairs)
	q.overlap(pairs)
	a, b = pairs[0], pairs[1]
	q.reopen(0) // Disable replacement while fault recovery is under observation.
	if err := q.ops().Stop(ctx, a.Primary, a.PrimaryUID); err != nil {
		t.Fatal(err)
	}
	waitLive(t, ctx, "primary loss reclaims its companion", 3*time.Minute, func(ctx context.Context) bool {
		q.reconcile(ctx)
		q.healthy(b)
		remaining, _ := q.r.DockerAllocations()
		return len(remaining) == 1 && remaining[0].ID == b.ID
	})
	q.assertGone([]DockerStatus{a})
	q.checkpoint("primary_vm_loss")
	if err := q.ops().Stop(ctx, b.Engine, b.EngineUID); err != nil {
		t.Fatal(err)
	}
	waitLive(t, ctx, "engine loss exits c2j and reclaims pair", 4*time.Minute, func(ctx context.Context) bool {
		q.reconcile(ctx)
		remaining, _ := q.r.DockerAllocations()
		return len(remaining) == 0
	})
	q.assertGone([]DockerStatus{b})
	q.checkpoint("engine_vm_loss")
	// Demonstrate real c2j jobs after both faulted generations; assert no stale
	// container/volume state or transport identity survived the slot reuse.
	q.reopen(2)
	pairs = q.ready()
	q.startJobs(pairs)
	q.complete(pairs)
	q.checkpoint("fresh_generation_after_faults")
	t.Log("DOCKER_QUALIFICATION_PASS: concurrency; isolation; crash recovery; bounded pressure; fresh generation")
}
