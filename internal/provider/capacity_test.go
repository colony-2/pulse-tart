package provider

import (
	"fmt"
	"testing"

	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-tart/internal/backend"
	"github.com/colony-2/pulse-tart/internal/config"
)

func countPointer(n int) *int { return &n }

func mixedCountFixture(t *testing.T, macDocker bool) (*Provider, *dockerFake) {
	t.Helper()
	p, f := dockerFixture(t, false)
	p.Config.Slots[0].Count = 2
	if !macDocker {
		p.Config.Slots[0].DockerProfile = ""
	}
	b := p.Config.Bases[0]
	b.Name, b.OS, b.OSVersion = "ubuntu-24.04-arm64", "linux", "24.04"
	b.Image = p.Config.Docker.Profiles[0].Image
	p.Config.Bases = append(p.Config.Bases, b)
	p.Config.Slots = append(p.Config.Slots, config.SlotGroup{Name: "linux", Base: b.Name, Count: 4, CPU: 4, MemoryMiB: 8192, DockerProfile: "engine"})
	return p, f
}

func warmCountPools(t *testing.T, p *Provider, want int) {
	t.Helper()
	for range 80 {
		if err := p.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, pair := range p.Store.data.Pairs {
			if pair.Phase == "ready" {
				n++
			}
		}
		if n == want {
			return
		}
	}
	t.Fatal("pool never reached expected ready count", want)
}

func TestDefaultJobCountsExcludeCompanionsAndBuilders(t *testing.T) {
	p, f := mixedCountFixture(t, true)
	warmCountPools(t, p, 6)
	mac, linux := 0, 0
	for _, v := range f.vms {
		if v.Stopped() {
			continue
		}
		if v.OS == "linux" {
			linux++
		} else {
			mac++
		}
	}
	if mac != 2 || linux != 6 {
		t.Fatalf("running VMs: macOS=%d Linux=%d; want 2 jobs + 4 jobs + 2 companions", mac, linux)
	}
	var launches []api.Launch
	for i := range 6 {
		l := dockerLaunch()
		l.LaunchID = fmt.Sprintf("job-%d", i)
		if i >= 2 {
			l.Base = "ubuntu*"
		}
		launches = append(launches, l)
	}
	got, err := p.Submit(t.Context(), launches)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range got {
		if result.Status != api.Accepted {
			t.Fatal("default capacity not usable", got)
		}
	}
	if len(p.Store.data.Records) != 6 || f.deliveries != 6 {
		t.Fatal("incorrect job count")
	}
	// Reopening must retain reservations even before the backend catches up.
	_ = p.Close()
	q, err := New(p.Config, f)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	extra := dockerLaunch()
	extra.LaunchID, extra.Base = "extra", "ubuntu*"
	got, err = q.Submit(t.Context(), []api.Launch{extra, launches[0]})
	if err != nil || got[0].Status != api.NoCapacity || got[1].Status != api.Accepted || f.deliveries != 6 {
		t.Fatal("replay or capacity after reopening", got, err)
	}
}

func TestMacDockerWorksWithLinuxJobCountZero(t *testing.T) {
	p, _ := dockerFixture(t, false)
	p.Config.VMCounts = config.VMCounts{MacOS: countPointer(1), Linux: countPointer(0), Total: countPointer(1)}
	pair := readyDocker(t, p)
	if pair.EngineUID == "" || pair.PrimaryUID == "" {
		t.Fatal("Linux job limit disabled a companion")
	}
	got, err := p.Submit(t.Context(), []api.Launch{dockerLaunch()})
	if err != nil || got[0].Status != api.Accepted {
		t.Fatal(got, err)
	}
}

func TestTotalCountRebalancesIdlePoolWithoutInterruptingJobs(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprint("active=", active), func(t *testing.T) {
			p, f := mixedCountFixture(t, false)
			p.Config.VMCounts.Total = countPointer(1)
			warmCountPools(t, p, 1) // Linux has a warm job VM; macOS has suspended sources.
			if active {
				l := dockerLaunch()
				l.Base = "ubuntu*"
				got, err := p.Submit(t.Context(), []api.Launch{l})
				if err != nil || got[0].Status != api.Accepted {
					t.Fatal(got, err)
				}
			}
			l := launch("mac-job")
			got, err := p.Submit(t.Context(), []api.Launch{l})
			if err != nil || got[0].Status != api.NoCapacity || p.Store.data.Records[l.LaunchID] != nil {
				t.Fatal("total limit did not defer admission", got, err)
			}
			for range 8 {
				if err := p.Reconcile(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			got, err = p.Submit(t.Context(), []api.Launch{l})
			want := api.Accepted
			if active {
				want = api.NoCapacity
				for _, pair := range p.Store.data.Pairs {
					if pair.LaunchID != "" && (pair.Phase != "running" || f.vms[pair.Primary.Name].Stopped()) {
						t.Fatal("rebalance interrupted an active job")
					}
				}
			}
			if err != nil || got[0].Status != want {
				t.Fatal("shared capacity did not rebalance", got, err)
			}
		})
	}
}

func TestLowerCountsDrainIdleWorkersAndPreserveActiveJob(t *testing.T) {
	p, f := mixedCountFixture(t, true)
	warmCountPools(t, p, 6)
	got, err := p.Submit(t.Context(), []api.Launch{dockerLaunch()})
	if err != nil || got[0].Status != api.Accepted {
		t.Fatal(got, err)
	}
	p.Config.VMCounts.Total = countPointer(0)
	for range 30 {
		if err := p.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for _, pair := range p.Store.data.Pairs {
		if pair.LaunchID == "" && pair.Phase != "gone" {
			t.Fatal("lower count retained idle allocation", pair.Phase)
		}
		if pair.LaunchID != "" && (pair.Phase != "running" || f.vms[pair.Primary.Name].Stopped()) {
			t.Fatal("lower count interrupted active allocation")
		}
	}
}

func TestMacCeilingIncludesForeignVMsAndBuilders(t *testing.T) {
	p, f := slotsFixture(t, 2)
	warmSlots(t, p)
	foreign := backend.VM{Spec: backend.Spec{Name: "foreign", OS: "darwin"}, UID: "foreign", Worker: p.Config.Worker, Status: "running"}
	f.vms[foreign.Name] = foreign
	f.vms["builder"] = backend.VM{Spec: backend.Spec{Name: "builder", OS: "darwin"}, UID: "builder", Worker: p.Config.Worker, Status: "pending"}
	got, err := p.Submit(t.Context(), []api.Launch{launch("one")})
	if err != nil || got[0].Status != api.NoCapacity {
		t.Fatal("third physical macOS VM allowed", got, err)
	}
	// Another worker's VM and a stopped local VM do not consume this worker's limit.
	v := f.vms["builder"]
	v.Worker = "elsewhere"
	f.vms["builder"] = v
	got, err = p.Submit(t.Context(), []api.Launch{launch("one"), launch("two")})
	if err != nil || got[0].Status != api.Accepted || got[1].Status != api.NoCapacity {
		t.Fatal("wrong per-worker physical capacity", got, err)
	}
	foreign.Conditions = []backend.Condition{{Type: "running", State: "false"}}
	f.vms[foreign.Name] = foreign
	got, err = p.Submit(t.Context(), []api.Launch{launch("two")})
	if err != nil || got[0].Status != api.Accepted {
		t.Fatal("stopped foreign VM consumed capacity", got, err)
	}
}

func TestUncertainDispatchRetainsJobCountWithoutInventory(t *testing.T) {
	p, f := slotsFixture(t, 2)
	warmSlots(t, p)
	p.Config.VMCounts.MacOS = countPointer(1)
	f.lose = true
	got, err := p.Submit(t.Context(), []api.Launch{launch("lost")})
	if err != nil || got[0].Status != api.Unknown {
		t.Fatal(got, err)
	}
	delete(f.vms, p.Store.data.Records["lost"].Name)
	got, err = p.Submit(t.Context(), []api.Launch{launch("second")})
	if err != nil || got[0].Status != api.NoCapacity {
		t.Fatal("uncertain dispatch released capacity", got, err)
	}
}

func TestSourcePreparationWaitsForPhysicalMacCapacity(t *testing.T) {
	p, f := fixture(t)
	f.baseVM = nil
	p.Store.data.Images = map[string]*ImageRecord{}
	p.Config.VMCounts.MacOS = countPointer(1)
	for _, name := range []string{"foreign-one", "foreign-two"} {
		f.vms[name] = backend.VM{Spec: backend.Spec{Name: name, OS: "darwin"}, Worker: p.Config.Worker, UID: name, Status: "running"}
	}
	if err := p.Reconcile(t.Context()); err != nil || f.creates != 0 {
		t.Fatal("builder started a third macOS instance", err)
	}
	delete(f.vms, "foreign-two")
	for range 12 {
		if err := p.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		mac := 0
		for _, vm := range f.vms {
			if vm.OS != "linux" && !vm.Stopped() {
				mac++
			}
		}
		if mac > 2 {
			t.Fatal("preparation exceeded hard macOS limit", mac)
		}
	}
	if f.creates != 3 {
		t.Fatal("job count incorrectly limited builders", f.creates)
	}
	for _, slot := range p.Store.data.Slots {
		if slot.Phase != "ready" {
			t.Fatal("serial template preparation did not finish", slot.Phase)
		}
	}
}
