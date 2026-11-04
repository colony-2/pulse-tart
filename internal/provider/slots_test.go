package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	compute "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-tart/internal/backend"
	"github.com/colony-2/pulse-tart/internal/config"
)

// Emulate guest preparation and the suspended state; job VMs stay pending.
func (f *fake) Events(_ context.Context, name string) ([]backend.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.vms[name]
	if !strings.HasPrefix(name, "pulse-template-") {
		return nil, nil
	}
	n := 0
	for _, c := range v.UID {
		n += int(c)
	}
	id := fmt.Sprintf(`PULSE_TEMPLATE_READY {"mac":"02:00:00:%02x:%02x:%02x","boot_id":"00000000-0000-0000-0000-%012x"}`, n>>16, (n>>8)&255, n&255, n)
	return []backend.Event{{Payload: id}}, nil
}
func (f *fake) Suspend(_ context.Context, name, uid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.vms[name]
	if !ok || v.UID != uid {
		return &backend.Error{Code: 404}
	}
	v.PowerState = "suspended"
	v.LocalName = "local-" + name
	v.Worker = v.Labels[config.WorkerNameLabel]
	v.Generation++
	v.ObservedGeneration = v.Generation
	v.Status = "running"
	v.Conditions = []backend.Condition{{Type: "running", State: "false"}}
	f.vms[name] = v
	return nil
}
func slotsFixture(t *testing.T, count int) (*Provider, *fake) {
	p, f := fixture(t)
	p.Config.Slots = []config.SlotGroup{{Name: "small", Count: count, CPU: 4, MemoryMiB: 8192, Base: p.Config.Bases[0].Name}}
	if err := func() error { _, err := config.Normalize(p.Config.Config); return err }(); err != nil {
		t.Fatal(err)
	}
	// Stable unique synthetic UIDs avoid relying on random test MACs.
	if err := p.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	i := 0
	for name, v := range f.vms {
		i++
		v.UID = fmt.Sprintf("slot-uid-%d", i)
		f.vms[name] = v
		for _, s := range p.Store.data.Slots {
			if s.Name == name {
				s.UID = v.UID
			}
		}
	}
	return p, f
}
func warmSlots(t *testing.T, p *Provider) {
	t.Helper()
	for range 3 {
		if err := p.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range p.Store.data.Slots {
		if s.Phase != "ready" {
			t.Fatalf("slot %s phase=%s", s.ID, s.Phase)
		}
	}
}
func stopJobs(f *fake) {
	for name, v := range f.vms {
		if strings.HasPrefix(name, "pulse-template-") {
			continue
		}
		v.Status = "failed"
		v.Conditions = []backend.Condition{{Type: "running", State: "false"}}
		f.vms[name] = v
	}
}
func TestSlotAdmissionAndRestart(t *testing.T) {
	p, f := slotsFixture(t, 2)
	out, _ := p.Submit(t.Context(), []compute.Launch{launch("warming")})
	if out[0].Status != compute.NoCapacity || f.creates != 2 {
		t.Fatal(out, f.creates)
	}
	warmSlots(t, p)
	out, err := p.Submit(t.Context(), []compute.Launch{launch("a"), launch("b"), launch("full")})
	if err != nil || out[0].Status != compute.Accepted || out[1].Status != compute.Accepted || out[2].Status != compute.NoCapacity {
		t.Fatal(out, err)
	}
	a, b := p.Store.data.Records["a"], p.Store.data.Records["b"]
	if a.SlotID == b.SlotID || a.SlotID == "" || f.vms[a.Name].Image == f.vms[b.Name].Image {
		t.Fatal("concurrent jobs share a template")
	}
	for _, r := range []*Record{a, b} {
		v := f.vms[r.Name]
		s := p.Store.data.Slots[r.SlotID]
		if v.CPU != 4 || v.Memory != 8192 || !v.Suspendable || v.Labels[config.WorkerNameLabel] != "mac-1" || !strings.Contains(v.StartupScript.Content, s.BootID) || !strings.Contains(v.StartupScript.Content, s.MAC) {
			t.Fatal("slot hardware, placement or restore guard missing")
		}
	}
	data, _ := json.Marshal(p.Store.data)
	if strings.Contains(string(data), "LEASE-SENTINEL") {
		t.Fatal("lease persisted")
	}
	p.Close()
	recovered, err := New(p.Config, f)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if err = recovered.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	out, _ = recovered.Submit(t.Context(), []compute.Launch{launch("a"), launch("still-full")})
	if out[0].Status != compute.Accepted || out[1].Status != compute.NoCapacity || f.creates != 4 {
		t.Fatal(out, f.creates)
	}
	stopJobs(f)
	if err = recovered.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	out, _ = recovered.Submit(t.Context(), []compute.Launch{launch("reuse")})
	if out[0].Status != compute.Accepted || len(recovered.Store.data.Slots) != 2 || f.creates != 5 {
		t.Fatal(out)
	}
}
func TestSlotUnknownDispatchRetainsReservation(t *testing.T) {
	p, f := slotsFixture(t, 1)
	warmSlots(t, p)
	f.lose = true
	out, _ := p.Submit(t.Context(), []compute.Launch{launch("lost")})
	if out[0].Status != compute.Unknown {
		t.Fatal(out)
	}
	delete(f.vms, p.Store.data.Records["lost"].Name)
	if err := p.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	out, _ = p.Submit(t.Context(), []compute.Launch{launch("other")})
	if out[0].Status != compute.NoCapacity || f.creates != 2 {
		t.Fatal(out)
	}
}
func TestTemplateUnknownCreateNeverReplayed(t *testing.T) {
	p, f := fixture(t)
	p.Config.Slots = []config.SlotGroup{{Name: "s", Count: 1, CPU: 4, MemoryMiB: 8192, Base: p.Config.Bases[0].Name}}
	f.lose = true
	if err := p.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	for name := range f.vms {
		delete(f.vms, name)
	}
	for range 3 {
		if err := p.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if f.creates != 1 {
		t.Fatal("ambiguous template replayed")
	}
}
func TestSlotResizeDrainsAndRebuilds(t *testing.T) {
	p, f := slotsFixture(t, 1)
	warmSlots(t, p)
	out, _ := p.Submit(t.Context(), []compute.Launch{launch("active")})
	if out[0].Status != compute.Accepted {
		t.Fatal(out)
	}
	old := p.Store.data.Slots["small-0001"].Name
	p.Config.Slots[0].CPU = 8
	if err := p.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.vms[old]; !ok {
		t.Fatal("busy template retired")
	}
	stopJobs(f)
	if err := p.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.vms[old]; ok {
		t.Fatal("idle old template retained")
	}
	if p.Store.data.Slots["small-0001"].Name == old {
		t.Fatal("template name reused")
	}
	warmSlots(t, p)
	out, _ = p.Submit(t.Context(), []compute.Launch{launch("new-size")})
	if out[0].Status != compute.Accepted || f.vms[p.Store.data.Records["new-size"].Name].CPU != 8 {
		t.Fatal(out)
	}
}
func TestSlotForeignCloneAndTemplateReplacement(t *testing.T) {
	p, f := slotsFixture(t, 1)
	warmSlots(t, p)
	s := p.Store.data.Slots["small-0001"]
	f.vms["foreign"] = backend.VM{Spec: backend.Spec{Name: "foreign", Image: s.LocalName}, UID: "foreign"}
	out, _ := p.Submit(t.Context(), []compute.Launch{launch("collision")})
	if out[0].Status != compute.NoCapacity {
		t.Fatal(out)
	}
	delete(f.vms, "foreign")
	v := f.vms[s.Name]
	v.UID = "replacement"
	f.vms[s.Name] = v
	if err := p.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s.Phase != "quarantine" || f.deletes != 0 {
		t.Fatal("replacement not quarantined")
	}
	out, _ = p.Submit(t.Context(), []compute.Launch{launch("blocked")})
	if out[0].Status != compute.NoCapacity {
		t.Fatal(out)
	}
}
func TestSlotSizingAndMissingTemplate(t *testing.T) {
	p, f := slotsFixture(t, 1)
	warmSlots(t, p)
	l := launch("too-big")
	l.CPUMillis = 4001
	out, _ := p.Submit(t.Context(), []compute.Launch{l})
	if out[0].Status != compute.Unsupported {
		t.Fatal(out)
	}
	old := p.Store.data.Slots["small-0001"].Name
	delete(f.vms, old)
	if err := p.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if p.Store.data.Slots["small-0001"].Name == old || f.creates != 2 {
		t.Fatal("missing unused template not rebuilt")
	}
}
func TestDuplicateTemplateMACQuarantined(t *testing.T) {
	p, f := slotsFixture(t, 2)
	// Same synthetic UID yields identical guest MAC evidence, while name identity stays distinct.
	for name, v := range f.vms {
		v.UID = "same-test-uid"
		f.vms[name] = v
		for _, s := range p.Store.data.Slots {
			if s.Name == name {
				s.UID = v.UID
			}
		}
	}
	for range 3 {
		if err := p.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	phases := map[string]int{}
	for _, s := range p.Store.data.Slots {
		phases[s.Phase]++
	}
	if phases["ready"] != 1 || phases["quarantine"] != 1 {
		t.Fatal(phases)
	}
}

func TestSlotDrainAndLegacyJournalRejection(t *testing.T) {
	p, f := slotsFixture(t, 1)
	warmSlots(t, p)
	p.Config.Slots[0].Count = 0
	if err := func() error { _, err := config.Normalize(p.Config.Config); return err }(); err != nil {
		t.Fatal(err)
	}
	if err := p.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(p.Store.data.Slots) != 0 || len(f.vms) != 0 {
		t.Fatal("drain retained templates")
	}
	out, _ := p.Submit(t.Context(), []compute.Launch{launch("drained")})
	if out[0].Status != compute.NoCapacity {
		t.Fatal(out)
	}
	p.Store.data.Version = 1
	if err := p.Store.save(); err != nil {
		t.Fatal(err)
	}
	p.Close()
	if q, err := New(p.Config, f); err == nil {
		q.Close()
		t.Fatal("legacy plugin journal accepted as a cell journal")
	}

}

func TestFailedTemplateHeldForInspection(t *testing.T) {
	p, f := slotsFixture(t, 1)
	s := p.Store.data.Slots["small-0001"]
	v := f.vms[s.Name]
	v.Status = "failed"
	v.Conditions = []backend.Condition{{Type: "running", State: "false"}}
	f.vms[s.Name] = v
	for range 3 {
		if err := p.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if s.Phase != "failed" || f.creates != 1 || f.deletes != 0 {
		t.Fatal("failed setup automatically replayed")
	}
	p.Config.Slots[0].Count = 0
	if err := p.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(p.Store.data.Slots) != 0 || f.deletes != 1 {
		t.Fatal("failed template could not drain")
	}
}

func TestMixedSlotSizesAndTemplateListing(t *testing.T) {
	p, f := slotsFixture(t, 2)
	p.Config.Slots[0].Count = 1
	p.Config.Slots = append(p.Config.Slots, config.SlotGroup{Name: "large", Count: 1, CPU: 8, MemoryMiB: 16384, Base: p.Config.Bases[0].Name})
	if err := func() error { _, err := config.Normalize(p.Config.Config); return err }(); err != nil {
		t.Fatal(err)
	}
	warmSlots(t, p)
	large := launch("large")
	large.CPUMillis = 6000
	large.MemoryBytes = 12288 << 20
	out, err := p.Submit(t.Context(), []compute.Launch{launch("small"), large})
	if err != nil || out[0].Status != compute.Accepted || out[1].Status != compute.Accepted {
		t.Fatal(out, err)
	}
	if p.Store.data.Records["small"].SlotID != "small-0001" || p.Store.data.Records["large"].SlotID != "large-0001" {
		t.Fatal("incorrect size selection")
	}
	v := f.vms[p.Store.data.Records["large"].Name]
	if v.CPU != 8 || v.Memory != 16384 {
		t.Fatal("clone resized below template size")
	}
	page, err := p.List(t.Context(), compute.ListRequest{})
	if err != nil || len(page.Items) != 2 {
		t.Fatal("templates leaked into job listing", page, err)
	}
}
