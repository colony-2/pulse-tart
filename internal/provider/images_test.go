package provider

import (
	"context"
	"strings"
	"testing"

	compute "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-tart/internal/backend"
	"github.com/colony-2/pulse-tart/internal/config"
)

func TestPrepareBaseOnceAndReuseSourcesAfterDrain(t *testing.T) {
	p, f := fixture(t)
	f.baseVM = nil
	p.Store.data.Images = map[string]*ImageRecord{}
	builds := 0
	p.Setup = func(context.Context) ([]byte, error) { builds++; return []byte("setup"), nil }
	for range 10 {
		if err := p.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if builds != 1 || f.creates != 3 {
		t.Fatalf("base prepared %d times; %d native creations", builds, f.creates)
	}
	for _, s := range p.Store.data.Slots {
		if s.Phase != "ready" {
			t.Fatal(s.Phase)
		}
	}
	base := p.Store.data.Images[config.BaseImageName(p.Config.OwnerID, p.Config.Bases[0].Name)]
	for _, s := range p.Store.data.Slots {
		if f.vms[s.Name].Image != base.LocalName {
			t.Fatal("slot did not use shared prepared base")
		}
	}
	p.Config.Slots[0].Count = 0
	if err := p.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(p.Store.data.Slots) != 0 || len(f.vms) != 3 {
		t.Fatal("drain removed reusable sources")
	}
	p.Config.C2JVersion = "different-version"
	p.Config.Slots[0].Count = 2
	for range 3 {
		if err := p.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if builds != 1 || f.creates != 3 {
		t.Fatal("existing source was rebuilt due to release selection")
	}
}
func TestJobStartupDoesNotCarryInstallerOrCachedIdentity(t *testing.T) {
	p, f := slotsFixture(t, 1)
	warmSlots(t, p)
	out, err := p.Submit(t.Context(), []compute.Launch{launch("new")})
	if err != nil || out[0].Status != compute.Accepted {
		t.Fatal(out, err)
	}
	script := f.vms[p.Store.data.Records["new"].Name].StartupScript.Content
	for _, bad := range []string{"PULSE_BUNDLE", "flake.nix", "install-guest.sh", p.Store.data.Slots["small-0001"].BootID} {
		if bad != "" && strings.Contains(script, bad) {
			t.Fatalf("startup contains %s", bad)
		}
	}
	if !strings.Contains(script, "/var/db/pulse-tart-template/ready") {
		t.Fatal("missing snapshot restore check")
	}
}

func TestSourceStoppedUsesObservedState(t *testing.T) {
	for _, tc := range []struct {
		power, running, local string
		want                  bool
	}{
		{"running", "false", "local-base", true}, // Orchard keeps desired power after shutdown.
		{"stopped", "false", "local-base", true},
		{"suspended", "false", "local-base", false},
		{"stopped", "true", "local-base", false},
		{"stopped", "", "local-base", false},
		{"stopped", "false", "", false},
	} {
		v := backend.VM{PowerState: tc.power, LocalName: tc.local, Conditions: []backend.Condition{{Type: "running", State: tc.running}}}
		if sourceStopped(v) != tc.want {
			t.Fatalf("source stopped: %+v", tc)
		}
	}
}

func TestDrainClearsFailedSourceProbeFallback(t *testing.T) {
	p, _ := slotsFixture(t, 1)
	warmSlots(t, p)
	for _, s := range p.Store.data.Slots {
		s.Phase = "fallback"
		s.UID = ""
	}
	p.Config.Slots[0].Count = 0
	if err := p.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(p.Store.data.Slots) != 0 {
		t.Fatal("drain retained a deleted source probe")
	}
}
