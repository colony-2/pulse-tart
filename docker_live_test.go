package tart

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-tart/internal/backend"
	"os"
	"strings"
	"testing"
	"time"
)

// Explicit opt-in: real public API, fresh paired provisioning and real JobDB.
func TestLiveDockerLibrary(t *testing.T) {
	kind := os.Getenv("PULSE_TEST_DOCKER_LIBRARY")
	if kind == "" {
		t.Skip("set PULSE_TEST_DOCKER_LIBRARY=orchard or tart")
	}
	osname := os.Getenv("PULSE_TEST_DOCKER_OS")
	if osname == "" {
		osname = "darwin"
	}
	base := DefaultBases()[0]
	if osname == "linux" {
		base = DefaultLinuxBase()
	} else if osname != "darwin" {
		t.Fatal("OS must be linux or darwin")
	}
	owner := fmt.Sprintf("docker-live-%d", time.Now().UnixNano())
	dir, e := os.MkdirTemp("", "pulse-docker-library-")
	if e != nil {
		t.Fatal(e)
	}
	endpoint := DockerEndpoint{Address: os.Getenv("PULSE_TEST_ENGINE_WORKER_ADDRESS"), PortMin: 45100, PortMax: 45120}
	cfg := Config{OwnerID: owner, StateDir: dir, Bases: []Base{base}, C2JVersion: os.Getenv("PULSE_TEST_C2J_VERSION"), Slots: []SlotGroup{{Name: "standard", Base: base.Name, Count: 1, DockerProfile: "standard"}}, Docker: &DockerConfig{Profiles: []DockerEngineProfile{DefaultDockerEngine()}}}
	factory := func() (*Runtime, error) {
		var r *Runtime
		var e error
		if kind == "orchard" {
			r, e = NewOrchard(cfg, OrchardConfig{URL: os.Getenv("PULSE_TEST_ORCHARD_URL"), Worker: os.Getenv("PULSE_TEST_ORCHARD_WORKER"), AllowInsecureHTTP: true, DockerEndpoint: endpoint})
		} else if kind == "tart" {
			r, e = NewTart(cfg, TartConfig{Home: os.Getenv("PULSE_TEST_TART_HOME"), DockerEndpoint: endpoint})
		} else {
			return nil, fmt.Errorf("invalid backend")
		}
		if e == nil {
			r.profile = "docker-integration"
		}
		return r, e
	}
	r, e := factory()
	if e != nil {
		t.Fatal(e)
	}
	defer func() { r.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Minute)
	defer cancel()
	report := newLiveReport(t, string(base.SSHPassword))
	connect := OrchardConfig{URL: os.Getenv("PULSE_TEST_ORCHARD_URL"), Worker: os.Getenv("PULSE_TEST_ORCHARD_WORKER"), AllowInsecureHTTP: true}
	t.Logf("backend=%s OS=%s private state=%s", kind, osname, dir)
	ops := r.p.Backend.(interface {
		Exec(context.Context, string, string, []byte) ([]byte, error)
		Stop(context.Context, string, string) error
	})
	defer func() {
		clean, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		report.capture(t, clean, kind, connect, r, dir)
		_ = r.Drain(clean)
		// Source builders contain no lease; explicit cleanup of this test's exact UIDs.
		images, _ := r.Images()
		for _, image := range images {
			if image.UID != "" {
				v, e := r.p.Backend.Get(clean, image.Name)
				if e == nil && v.UID == image.UID && !v.Stopped() {
					_ = ops.Stop(clean, v.Name, v.UID)
				}
			}
		}
		for clean.Err() == nil {
			_ = r.Reconcile(clean)
			pairs, _ := r.DockerAllocations()
			slots, _ := r.Slots()
			if len(pairs) == 0 && len(slots) == 0 {
				if e := r.PurgeImages(clean); e != nil {
					t.Errorf("purge: %v", e)
				}
				return
			}
			time.Sleep(time.Second)
		}
		t.Errorf("cleanup incomplete; preserve private state %s", dir)
	}()
	waitLive(t, ctx, "Docker ready capacity", 30*time.Minute, func(ctx context.Context) bool {
		if e := r.Reconcile(ctx); e != nil {
			t.Logf("reconcile: %v", e)
		}
		images, _ := r.Images()
		for _, im := range images {
			if im.Phase == "failed" || im.Phase == "quarantine" {
				events, _ := r.p.Backend.(interface {
					Events(context.Context, string) ([]backend.Event, error)
				}).Events(ctx, im.Name)
				for _, ev := range events {
					if len(ev.Payload) > 0 {
						t.Log(ev.Payload)
					}
				}
				t.Fatalf("image %s: %s", im.Name, im.Phase)
			}
		}
		pairs, _ := r.DockerAllocations()
		if len(pairs) > 0 {
			t.Logf("Docker phase: %s", pairs[0].Phase)
			if pairs[0].Phase == "reclaiming" || pairs[0].Phase == "quarantine" {
				for _, name := range []string{pairs[0].Primary, pairs[0].Engine} {
					if name == "" {
						continue
					}
					v, e := r.p.Backend.Get(ctx, name)
					if e == nil {
						t.Logf("failed allocation %s: %s", name, v.Status)
					}
					events, _ := r.p.Backend.(interface {
						Events(context.Context, string) ([]backend.Event, error)
					}).Events(ctx, name)
					for _, event := range events {
						if event.Payload != "" {
							t.Log(event.Payload)
						}
					}
				}
				t.Fatal("allocation failed before admission")
			}
		}
		return len(pairs) == 1 && pairs[0].Phase == "ready"
	})
	caps, e := r.GetCapabilities(ctx)
	if e != nil || len(caps.Bases) != 1 || !strings.Contains(strings.Join(caps.Bases[0].Capabilities, ","), "docker") {
		t.Fatal("Docker discovery failed", e)
	}
	pairs, _ := r.DockerAllocations()
	primary := pairs[0]
	facts, e := ops.Exec(ctx, primary.Primary, primary.PrimaryUID, []byte(dockerVersionProbe))
	if e != nil {
		t.Fatal("version inventory", e)
	}
	var versions map[string]any
	if json.Unmarshal(facts, &versions) != nil {
		t.Fatal("invalid version inventory")
	}
	report.write(t, "docker-library-"+osname+"-versions.json", versions)
	contract, e := os.ReadFile("scripts/docker-prototype/engine-contract.sh")
	if e != nil {
		t.Fatal(e)
	}
	decode := "-D"
	if osname == "linux" {
		decode = "-d"
	}
	setup := fmt.Sprintf("set -eu\nsudo -n mkdir -p /opt/pulse-docker\nprintf '%%s' '%s' | base64 %s | sudo -n tee /opt/pulse-docker/engine-contract.sh >/dev/null\nsudo -n chmod 755 /opt/pulse-docker /opt/pulse-docker/engine-contract.sh\n", base64.StdEncoding.EncodeToString(contract), decode)
	if osname == "darwin" {
		setup += strings.ReplaceAll(fixtureStartup, "pulse-orchard-fixture", "pulse-docker-fixture")
	} else {
		setup += "sudo -n systemd-run --unit=pulse-docker-fixture --setenv=HOME=/root --setenv=PATH=/opt/pulse-tart/bin:/usr/bin:/bin /opt/pulse-tart/bin/pulse-docker-fixture\n"
	}
	if _, e = ops.Exec(ctx, primary.Primary, primary.PrimaryUID, []byte(setup)); e != nil {
		t.Fatal("fixture setup", e)
	}
	fetch := func(path string, out any) bool {
		data, e := ops.Exec(ctx, primary.Primary, primary.PrimaryUID, []byte("curl -fsS 'http://127.0.0.1:18881/"+path+"'\n"))
		return e == nil && json.Unmarshal(data, out) == nil
	}
	waitLive(t, ctx, "JobDB fixture ready", time.Minute, func(context.Context) bool { var s map[string]any; return fetch("health", &s) })
	var launch api.Launch
	if !fetch("launch?case=valid", &launch) {
		t.Fatal("fixture lease unavailable")
	}
	report.addLease(launch.Lease)
	launch.LaunchID = owner
	launch.Allocation = api.Allocation{Base: base.Name, Capabilities: []string{"docker", "git"}, CPUMillis: 1000, MemoryBytes: 2 << 30}
	out, e := r.Submit(ctx, []api.Launch{launch})
	if e != nil || out[0].Status != api.Accepted {
		t.Fatal("Docker submit", out, e)
	}
	page, e := r.List(ctx, api.ListRequest{LaunchID: launch.LaunchID})
	want := 2
	if osname == "linux" {
		want = 1
	}
	if e != nil || len(page.Items) != want {
		t.Fatal("paired listing", page, e)
	}
	if e = r.Close(); e != nil {
		t.Fatal(e)
	}
	r, e = factory()
	if e != nil {
		t.Fatal(e)
	}
	ops = r.p.Backend.(interface {
		Exec(context.Context, string, string, []byte) ([]byte, error)
		Stop(context.Context, string, string) error
	})
	out, e = r.Submit(ctx, []api.Launch{launch})
	if e != nil || out[0].Status != api.Accepted {
		t.Fatal("reopened duplicate", out, e)
	}
	var status fixtureStatus
	waitLive(t, ctx, "real Docker c2j job", 8*time.Minute, func(context.Context) bool {
		if !fetch("status?case=valid", &status) {
			return false
		}
		if status.Status == "FAILED" {
			t.Fatal("c2j job failed")
		}
		return status.Status == "COMPLETED"
	})
	report.write(t, "docker-library-"+osname+"-jobdb.json", status)
	if !status.OutputOK || status.Claims != 1 || status.Renewals < 2 || status.SecretCalls != 1 {
		t.Fatal("JobDB execution facts", status)
	}
	cfg.Slots[0].Count = 0
	_ = r.Drain(ctx)
	waitLive(t, ctx, "paired cleanup", 3*time.Minute, func(ctx context.Context) bool {
		if e := r.Reconcile(ctx); e != nil {
			t.Log(e)
			return false
		}
		pairs, _ := r.DockerAllocations()
		return len(pairs) == 0
	})
	t.Logf("DOCKER_LIBRARY_JOBDB_PASS: %s; completed; one claim; renewals=%d; paired cleanup passed", osname, status.Renewals)
}

const dockerVersionProbe = `sudo -n /opt/pulse-tart/bin/python3 - <<'PULSE_VERSIONS'
import json,platform,subprocess
run=lambda *args:subprocess.check_output(args,text=True).strip()
p='/opt/pulse-tart/bin/'
print(json.dumps({'os':platform.system(),'architecture':platform.machine(),
 'c2j':run(p+'c2j','version'),
 'docker':json.loads(run(p+'docker','--host','unix:///var/run/docker.sock','version','--format','{{json .}}')),
 'buildx':run(p+'docker','buildx','version'),
 'compose':run(p+'docker','compose','version')}))
PULSE_VERSIONS
`
