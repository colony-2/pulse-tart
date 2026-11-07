package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-tart/internal/backend"
	"github.com/colony-2/pulse-tart/internal/config"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

type dockerFake struct {
	*fake
	deliveries                                  int
	receipt, loseDelivery, failDelete, notReady bool
	payload                                     envelope
}

func (f *dockerFake) Endpoint(context.Context, string, string, int) (bool, error) { return true, nil }
func (f *dockerFake) Events(ctx context.Context, name string) ([]backend.Event, error) {
	if strings.HasSuffix(name, "-engine") || strings.HasSuffix(name, "-primary") {
		return []backend.Event{{Payload: "PULSE_DOCKER_ENGINE_READY private-engine"}}, nil
	}
	return f.fake.Events(ctx, name)
}
func (f *dockerFake) Exec(_ context.Context, name, uid string, script []byte) ([]byte, error) {
	s := string(script)
	if strings.Contains(s, "| sudo -n /opt/pulse-tart/bin/pulse-tart-guest ingest") {
		f.deliveries++
		f.receipt = true
		encoded := strings.Split(strings.Split(s, "printf '%s' '")[1], "'")[0]
		data, _ := base64.StdEncoding.DecodeString(encoded)
		_ = json.Unmarshal(data, &f.payload)
		if f.loseDelivery {
			return nil, errors.New("lost handoff response")
		}
		return []byte("PULSE_HANDOFF_OK"), nil
	}
	if strings.Contains(s, "/var/db/pulse-tart/fingerprint") {
		if f.receipt {
			return []byte("PULSE_HANDOFF_OK"), nil
		}
		return nil, errors.New("no receipt")
	}
	if strings.Contains(s, "PULSE_DOCKER_PRIMARY_READY") {
		if f.notReady {
			return nil, errors.New("engine unavailable")
		}
		return []byte("PULSE_DOCKER_PRIMARY_READY"), nil
	}
	return nil, nil
}
func (f *dockerFake) Stop(_ context.Context, name, uid string) error {
	v := f.vms[name]
	if v.UID != uid {
		return errors.New("wrong UID")
	}
	v.Status = "failed"
	v.PowerState = "stopped"
	v.Conditions = []backend.Condition{{Type: "running", State: "false"}}
	f.vms[name] = v
	return nil
}
func (f *dockerFake) Delete(ctx context.Context, name string) error {
	if f.failDelete {
		return errors.New("delete unavailable")
	}
	return f.fake.Delete(ctx, name)
}
func dockerFixture(t *testing.T, linux bool) (*Provider, *dockerFake) {
	p, f := fixture(t)
	d := &dockerFake{fake: f}
	p.Backend = d
	p.Config.Docker = &config.Docker{ReadyTimeout: time.Minute, Profiles: []config.EngineProfile{{Name: "engine", Image: "example.test/linux@sha256:" + strings.Repeat("b", 64), SSHUsername: "admin", SSHPassword: "admin", CPU: 4, MemoryMiB: 8192, DiskGiB: 32, EngineVersion: "29.1.3"}}}
	p.Config.DockerEndpoint = config.Endpoint{Address: "192.0.2.1", PortMin: 45000, PortMax: 45010}
	p.Config.Slots = []config.SlotGroup{{Name: "docker", Base: p.Config.Bases[0].Name, Count: 1, CPU: 4, MemoryMiB: 8192, DockerProfile: "engine"}}
	if linux {
		p.Config.Bases[0].OS = "linux"
		p.Config.Bases[0].OSVersion = "24.04"
	}
	p.Assets = fstest.MapFS{"scripts/docker-engine.sh": &fstest.MapFile{Data: []byte("engine installer")}, "scripts/docker-primary.sh": &fstest.MapFile{Data: []byte("primary setup")}}
	return p, d
}
func readyDocker(t *testing.T, p *Provider) *PairRecord {
	t.Helper()
	for range 30 {
		if e := p.Reconcile(t.Context()); e != nil {
			t.Fatal(e)
		}
		for _, pair := range p.Store.data.Pairs {
			if pair.Phase == "ready" {
				return pair
			}
		}
	}
	t.Fatal("pair never ready")
	return nil
}
func dockerLaunch() api.Launch {
	l := launch("docker-job")
	l.Capabilities = []string{"docker", "git"}
	l.MemoryBytes = 7 << 30
	return l
}
func TestDockerPairHandoffReopenAndCompleteOwnership(t *testing.T) {
	p, f := dockerFixture(t, false)
	pair := readyDocker(t, p)
	l := dockerLaunch()
	l.Lease = api.Lease(string([]byte{0xff, 0, 0xfe, 'x'}))
	f.loseDelivery = true
	out, e := p.Submit(t.Context(), []api.Launch{l})
	if e != nil || out[0].Status != api.Unknown || f.deliveries != 1 {
		t.Fatal(out, e)
	}
	if string(f.payload.Process.StdinBytes) != string(l.Lease) {
		t.Fatal("opaque lease changed")
	}
	if !strings.Contains(strings.Join(f.payload.Process.Args, " "), "--execution-memory 7680Mi") {
		t.Fatal("primary headroom missing")
	}
	if strings.Contains(pair.Engine.StartupScript.Content, base64.StdEncoding.EncodeToString([]byte(l.Lease))) {
		t.Fatal("lease entered companion")
	}
	if e = p.Close(); e != nil {
		t.Fatal(e)
	}
	store, e := OpenStore(p.Config.StateDir, p.Config.Scope())
	if e != nil {
		t.Fatal(e)
	}
	p.Store = store
	out, e = p.Submit(t.Context(), []api.Launch{l})
	if e != nil || out[0].Status != api.Unknown || f.deliveries != 1 {
		t.Fatal("replayed uncertain handoff")
	}
	if e = p.Reconcile(t.Context()); e != nil {
		t.Fatal(e)
	}
	out, e = p.Submit(t.Context(), []api.Launch{l})
	if e != nil || out[0].Status != api.Accepted || f.deliveries != 1 {
		t.Fatal("receipt not recovered", out, e)
	}
	page, e := p.List(t.Context(), api.ListRequest{LaunchID: l.LaunchID, PageSize: 1})
	if e != nil || len(page.Items) != 1 || page.NextPageToken == "" {
		t.Fatal("missing paired listing", page, e)
	}
	page2, e := p.List(t.Context(), api.ListRequest{LaunchID: l.LaunchID, PageSize: 1, PageToken: page.NextPageToken})
	if e != nil || len(page2.Items) != 1 || page.Items[0].ID == page2.Items[0].ID {
		t.Fatal(page2, e)
	}
	pair = p.Store.data.Pairs[pair.ID]
	_ = f.Stop(t.Context(), pair.Primary.Name, pair.PrimaryUID)
	f.failDelete = true
	for range 3 {
		_ = p.Reconcile(t.Context())
	}
	if pair.Phase == "gone" || !p.slotBusy(pair.SlotID) {
		t.Fatal("capacity freed before teardown")
	}
	page, e = p.List(t.Context(), api.ListRequest{LaunchID: l.LaunchID})
	if e != nil || len(page.Items) != 2 {
		t.Fatal("stopped primary disappeared from ownership", page, e)
	}
	f.failDelete = false
	p.Config.Slots[0].Count = 0
	for range 10 {
		if e = p.Reconcile(t.Context()); e != nil {
			t.Fatal(e)
		}
	}
	if pair.Phase != "gone" || p.Store.data.Records[l.LaunchID].Phase != "done" {
		t.Fatal("pair not reaped", pair.Phase)
	}
}
func TestDockerLostCreateNeverDispatchesTwice(t *testing.T) {
	p, f := dockerFixture(t, false)
	f.lose = true
	// Lost responses still leave native resources discoverable by their exact hashes.
	pair := readyDocker(t, p)
	if pair.EngineUID == "" || pair.PrimaryUID == "" {
		t.Fatal("did not recover native identities")
	}
	before := f.creates
	for range 3 {
		if e := p.Reconcile(t.Context()); e != nil {
			t.Fatal(e)
		}
	}
	if f.creates != before {
		t.Fatal("duplicate native creation")
	}
}
func TestDockerReadinessAndPrimaryMinimumDeclineBeforeLease(t *testing.T) {
	p, f := dockerFixture(t, false)
	readyDocker(t, p)
	l := dockerLaunch()
	l.MemoryBytes = 8 << 30
	out, e := p.Submit(t.Context(), []api.Launch{l})
	if e != nil || out[0].Status != api.Unsupported || f.deliveries != 0 {
		t.Fatal(out, e)
	}
	l.MemoryBytes = 7 << 30
	f.notReady = true
	out, e = p.Submit(t.Context(), []api.Launch{l})
	if e != nil || out[0].Status != api.Unavailable || f.deliveries != 0 {
		t.Fatal(out, e)
	}
}
func TestDockerZeroTimeoutAndForeignIdentityAreNotReaped(t *testing.T) {
	p, f := dockerFixture(t, false)
	pair := readyDocker(t, p)
	l := dockerLaunch()
	out, _ := p.Submit(t.Context(), []api.Launch{l})
	if out[0].Status != api.Accepted {
		t.Fatal(out)
	}
	pair.Created = time.Now().Add(-100 * time.Hour)
	p.Config.Slots[0].Count = 0
	if e := p.Reconcile(t.Context()); e != nil {
		t.Fatal(e)
	}
	if pair.Phase != "running" {
		t.Fatal("zero-timeout job reaped by age")
	}
	v := f.vms[pair.Engine.Name]
	v.UID = "foreign"
	f.vms[pair.Engine.Name] = v
	if e := p.Reconcile(t.Context()); e == nil {
		t.Fatal("foreign identity accepted")
	}
	if pair.Phase != "quarantine" || f.deletes != 0 {
		t.Fatal("foreign VM deleted")
	}
}
func TestDockerLinuxOwnsOnlyPrimary(t *testing.T) {
	p, f := dockerFixture(t, true)
	pair := readyDocker(t, p)
	if pair.Engine.Name != "" || pair.Port != 0 || pair.Primary.Suspendable {
		t.Fatal("Linux got a companion or suspension")
	}
	out, e := p.Submit(t.Context(), []api.Launch{dockerLaunch()})
	if e != nil || out[0].Status != api.Accepted {
		t.Fatal(out, e)
	}
	if !strings.Contains(strings.Join(f.payload.Process.Args, " "), "linux/arm64") || f.payload.Process.WorkingDir != "/home/pulseworker" {
		t.Fatal("wrong guest process")
	}
}

func TestDockerRecoveryAtEveryPreparationBoundary(t *testing.T) {
	for _, phase := range []string{"reserved", "engine-dispatch", "primary-dispatch", "configuring", "ready"} {
		t.Run(phase, func(t *testing.T) {
			p, f := dockerFixture(t, false)
			f.notReady = phase == "configuring"
			var pair *PairRecord
			for range 30 {
				if e := p.Reconcile(t.Context()); e != nil {
					t.Fatal(e)
				}
				for _, v := range p.Store.data.Pairs {
					if v.Phase == phase {
						pair = v
					}
				}
				if pair != nil {
					break
				}
			}
			if pair == nil {
				t.Fatal("boundary not reached")
			}
			if e := p.Close(); e != nil {
				t.Fatal(e)
			}
			s, e := OpenStore(p.Config.StateDir, p.Config.Scope())
			if e != nil {
				t.Fatal(e)
			}
			p.Store = s
			f.notReady = false
			readyDocker(t, p)
			counts := map[string]int{}
			for _, v := range f.vms {
				for _, role := range []string{"primary", "engine"} {
					if strings.HasSuffix(v.Name, "-"+role) {
						counts[role]++
					}
				}
				for key := range v.Labels {
					if key != "org.cirruslabs.orchard.worker-name" {
						t.Fatalf("ownership became an Orchard worker selector: %s", key)
					}
				}
			}
			if counts["primary"] != 1 || counts["engine"] != 1 {
				t.Fatal("reopen duplicated native resources", counts)
			}
		})
	}
}
func TestDockerPartialUnknownCreateReclaimsKnownCompanion(t *testing.T) {
	p, f := dockerFixture(t, false)
	pair := readyDocker(t, p)
	// Simulate a lost primary response followed by absence, before any lease existed.
	delete(f.vms, pair.Primary.Name)
	pair.PrimaryUID = ""
	pair.Phase = "reclaiming"
	for range 6 {
		if e := p.reconcilePairs(t.Context()); e != nil {
			t.Fatal(e)
		}
	}
	if _, exists := f.vms[pair.Engine.Name]; exists {
		t.Fatal("known companion leaked")
	}
	if pair.Phase == "gone" || !p.slotBusy(pair.SlotID) {
		t.Fatal("unknown primary reservation released")
	}
}
func TestDockerReadOnlyListErrorsOnUnavailableInventory(t *testing.T) {
	p, f := dockerFixture(t, false)
	pair := readyDocker(t, p)
	out, _ := p.Submit(t.Context(), []api.Launch{dockerLaunch()})
	if out[0].Status != api.Accepted {
		t.Fatal(out)
	}
	f.readFail = true
	if _, e := p.List(t.Context(), api.ListRequest{}); e == nil {
		t.Fatal("inventory failure became empty list")
	}
	if !p.slotBusy(pair.SlotID) {
		t.Fatal("lost reservation")
	}
}

func TestDockerProfileChangeRetiresOrdinaryTemplateIdentity(t *testing.T) {
	p, _ := dockerFixture(t, false)
	targets, e := p.slotTargets()
	if e != nil {
		t.Fatal(e)
	}
	docker := targets["docker-0001"]
	p.Config.Slots[0].DockerProfile = ""
	targets, e = p.slotTargets()
	if e != nil {
		t.Fatal(e)
	}
	ordinary := targets["docker-0001"]
	if docker.hash == ordinary.hash || docker.spec.Name == ordinary.spec.Name {
		t.Fatal("Docker profile reused ordinary template identity")
	}
}

func TestDockerPoolGrowthKeepsExistingReadyAllocation(t *testing.T) {
	p, _ := dockerFixture(t, false)
	pair := readyDocker(t, p)
	p.Config.Slots[0].Count++
	for range 3 {
		if e := p.Reconcile(t.Context()); e != nil {
			t.Fatal(e)
		}
	}
	if pair.Phase != "ready" {
		t.Fatalf("pool growth discarded warm allocation: %s", pair.Phase)
	}
	out, e := p.Submit(t.Context(), []api.Launch{dockerLaunch()})
	if e != nil || out[0].Status != api.Accepted {
		t.Fatal("existing capacity lost on growth", out, e)
	}
}
