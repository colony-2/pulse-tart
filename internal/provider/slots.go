package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	compute "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-tart/internal/backend"
	"github.com/colony-2/pulse-tart/internal/config"
)

type templateBackend interface {
	Suspend(context.Context, string, string) error
	Events(context.Context, string) ([]backend.Event, error)
}

type SlotRecord struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	UID        string    `json:"uid"`
	NativeHash string    `json:"native_hash"`
	ConfigHash string    `json:"config_hash"`
	Phase      string    `json:"phase"`
	Worker     string    `json:"worker"`
	LocalName  string    `json:"local_name"`
	MAC        string    `json:"mac"`
	BootID     string    `json:"boot_id"`
	Created    time.Time `json:"created"`
	Probe      bool      `json:"probe,omitempty"`
}

type slotTarget struct {
	spec   backend.Spec
	image  string
	worker string
	hash   string
}

func validSlotPhase(s string) bool {
	switch s {
	case "dispatch", "preparing", "suspending", "ready", "deleting", "quarantine", "failed", "fallback":
		return true
	}
	return false
}

func (p *Provider) slotTargets() (map[string]slotTarget, error) {
	targets := map[string]slotTarget{}
	for _, group := range p.Config.Slots {
		img := p.base(group.Base)
		if img.OS == "linux" {
			continue
		}
		spec := p.sourceSpec(img, group.CPU, group.MemoryMiB)
		if image := p.Store.data.Images[p.baseImageName(img.Name)]; image != nil && image.Phase == "ready" {
			spec.Image = image.LocalName
		} else {
			spec.Image = ""
		}
		spec.StartupScript.Content = templateStartup(int(p.Config.BootstrapTimeout.Seconds()), img)
		target := slotTarget{spec: spec, image: group.Base, worker: p.Config.Worker, hash: p.Store.hash(struct {
			Base          config.Base
			CPU, Memory   int64
			Group         string
			DockerProfile string `json:",omitempty"`
		}{img, group.CPU, group.MemoryMiB, group.Name, group.DockerProfile})}
		for i := 0; i < group.Count; i++ {
			t := target
			namingGroup := group
			if group.DockerProfile != "" {
				namingGroup.Name += "-docker-" + group.DockerProfile
			}
			t.spec.Name = config.SlotImageName(p.Config.OwnerID, namingGroup, i+1)
			targets[fmt.Sprintf("%s-%04d", group.Name, i+1)] = t
		}
	}
	return targets, nil
}
func templateStartup(seconds int, base config.Base) string {
	return fmt.Sprintf("#!/bin/bash\nset -eu\nsudo -n /bin/bash /opt/pulse-tart-image/prepare-template.sh %d %s '%s'\n", seconds, base.OSVersion, strings.Join(base.Capabilities, " "))
}
func (p *Provider) cacheSlot(s *SlotRecord) {
	p.Store.data.Images[s.Name] = &ImageRecord{Name: s.Name, UID: s.UID, NativeHash: s.NativeHash, Phase: "ready", LocalName: s.LocalName, Worker: s.Worker, Kind: "slot", Created: s.Created}
}

func (p *Provider) slotBusy(id string) bool {
	for _, pair := range p.Store.data.Pairs {
		if pair.SlotID == id && pair.Phase != "gone" {
			return true
		}
	}
	for _, r := range p.Store.data.Records {
		if r.SlotID == id && r.Phase != "done" {
			return true
		}
	}
	return false
}
func (p *Provider) matchesSlot(s *SlotRecord, v backend.VM) bool {
	return v.Name == s.Name && v.UID != "" && (s.UID == "" || s.UID == v.UID) && p.Store.hash(normalized(v.Spec)) == s.NativeHash && (v.Worker == "" || v.Worker == s.Worker)
}
func suspended(v backend.VM) bool {
	if v.PowerState != "suspended" || !v.Stopped() || v.Generation != v.ObservedGeneration || v.LocalName == "" {
		return false
	}
	for _, c := range v.Conditions {
		if c.Type == "suspending" && c.State == "true" {
			return false
		}
	}
	return true
}

var bootIDPattern = regexp.MustCompile(`^[0-9A-Fa-f]{8}(-[0-9A-Fa-f]{4}){3}-[0-9A-Fa-f]{12}$`)

func validSlotIdentity(s *SlotRecord) bool {
	if s.Worker == "" {
		return false
	}
	if s.MAC != "" || s.BootID != "" {
		mac, err := net.ParseMAC(s.MAC)
		if err != nil || len(mac) != 6 || mac.String() != s.MAC || !bootIDPattern.MatchString(s.BootID) {
			return false
		}
	}
	return s.Phase != "ready" || (s.UID != "" && s.LocalName != "")
}

func templateIdentity(events []backend.Event) (mac, boot string, ok bool) {
	for _, e := range events {
		for _, line := range strings.Split(e.Payload+"\n"+e.Message, "\n") {
			const prefix = "PULSE_TEMPLATE_READY "
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			var identity struct {
				MAC    string `json:"mac"`
				BootID string `json:"boot_id"`
			}
			if json.Unmarshal([]byte(strings.TrimPrefix(line, prefix)), &identity) != nil || !bootIDPattern.MatchString(identity.BootID) {
				continue
			}
			m, err := net.ParseMAC(identity.MAC)
			if err != nil || len(m) != 6 || m[0]&1 != 0 {
				continue
			}
			return m.String(), identity.BootID, true
		}
	}
	return "", "", false
}

// Called with the journal lock held. Never waits for guest preparation or resume.
func (p *Provider) reconcileSlots(ctx context.Context) error {
	if len(p.Config.Slots) == 0 && len(p.Store.data.Slots) == 0 {
		return nil
	}
	b, ok := p.Backend.(templateBackend)
	if !ok {
		return ErrBackend
	}
	targets, err := p.slotTargets()
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(p.Store.data.Slots))
	for id := range p.Store.data.Slots {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s := p.Store.data.Slots[id]
		if s.Phase == "fallback" {
			if _, wanted := targets[id]; !wanted && !p.slotBusy(id) {
				delete(p.Store.data.Slots, id)
			}
			continue
		}
		if s.Phase == "quarantine" {
			continue
		}
		v, err := p.Backend.Get(ctx, s.Name)
		if backend.IsStatus(err, 404) {
			// An ambiguous POST retains its slot forever: absence cannot prove no dispatch.
			if s.UID != "" && !p.slotBusy(id) {
				delete(p.Store.data.Slots, id)
			}
			continue
		}
		if err != nil {
			return ErrBackend
		}
		if !p.matchesSlot(s, v) {
			s.Phase = "quarantine"
			continue
		}
		s.UID = v.UID
		t, wanted := targets[id]
		retire := !wanted || s.ConfigHash != t.hash || s.Phase == "deleting"
		if retire {
			if p.slotBusy(id) {
				continue
			}
			if s.Phase == "ready" && strings.HasPrefix(s.Name, "pulse-") && !strings.HasPrefix(s.Name, "pulse-template-") {
				p.cacheSlot(s)
				delete(p.Store.data.Slots, id)
				continue
			}
			// Legacy or incomplete builders are owned by the journal, not published sources.
			if !v.Stopped() {
				if err = b.Suspend(ctx, s.Name, s.UID); err != nil {
					return ErrBackend
				}
				continue
			}
			s.Phase = "deleting"
			if err = p.Store.save(); err != nil {
				return err
			}
			// The GET above verified immutable ownership immediately before this deletion.
			if err = p.Backend.Delete(ctx, s.Name); err != nil && !backend.IsStatus(err, 404) {
				return ErrBackend
			}
			if _, err = p.Backend.Get(ctx, s.Name); backend.IsStatus(err, 404) {
				delete(p.Store.data.Slots, id)
			} else if err != nil {
				return ErrBackend
			}
			continue
		}
		if v.Status == "failed" && v.Stopped() {
			if s.Probe {
				if err := p.Backend.Delete(ctx, s.Name); err != nil {
					return err
				}
				s.Probe = false
				s.Phase = "fallback"
				s.UID = ""
				if err := p.Store.save(); err != nil {
					return err
				}
				continue
			}
			s.Phase = "failed"
			continue
		}
		if s.Phase == "ready" {
			if !suspended(v) || s.LocalName != v.LocalName || v.Worker != s.Worker {
				s.Phase = "quarantine"
			}
			continue
		}
		if s.MAC == "" {
			s.Phase = "preparing"
			events, err := b.Events(ctx, s.Name)
			if err != nil {
				return ErrBackend
			}
			mac, boot, ready := templateIdentity(events)
			if !ready {
				continue
			}
			for otherID, other := range p.Store.data.Slots {
				if otherID != id && other.MAC == mac {
					s.Phase = "quarantine"
					break
				}
			}
			if s.Phase == "quarantine" {
				continue
			}
			s.MAC = mac
			s.BootID = boot
			s.Phase = "suspending"
			if err = p.Store.save(); err != nil {
				return err
			}
		}
		if suspended(v) {
			if v.Worker != s.Worker {
				s.Phase = "quarantine"
				continue
			}
			if err := p.retain(ctx, s.Name, s.UID); err != nil {
				return err
			}
			s.LocalName = v.LocalName
			s.Phase = "ready"
			p.cacheSlot(s)
		} else {
			s.Phase = "suspending"
			if err = p.Store.save(); err != nil {
				return err
			}
			if err = b.Suspend(ctx, s.Name, s.UID); err != nil {
				return ErrBackend
			}
		}
	}
	// Persist removals/quarantines before any new template dispatch.
	if err = p.Store.save(); err != nil {
		return err
	}
	ids = ids[:0]
	for id := range targets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		old := p.Store.data.Slots[id]
		fallback := old != nil && old.Phase == "fallback"
		if (old != nil && !fallback) || p.slotBusy(id) {
			continue
		}
		t := targets[id]
		// Reuse the published source by name, even when it was built by the
		// standalone script or replaced since this journal last observed it.
		existing, err := p.adopt(ctx, t.spec, p.base(t.image))
		if err == nil {
			if !suspended(existing) || existing.Worker != t.worker || existing.CPU != t.spec.CPU || existing.Memory != t.spec.Memory {
				return fmt.Errorf("slot source %s is not suspended or has incompatible hardware", t.spec.Name)
			}
			if err := p.retain(ctx, existing.Name, existing.UID); err != nil {
				return err
			}
			s := &SlotRecord{ID: id, Name: existing.Name, UID: existing.UID, NativeHash: p.Store.hash(normalized(existing.Spec)), ConfigHash: t.hash, Phase: "ready", Worker: t.worker, LocalName: existing.LocalName, Created: time.Now().UTC()}
			p.Store.data.Slots[id] = s
			p.cacheSlot(s)
			if err := p.Store.save(); err != nil {
				return err
			}
			continue
		}
		if !backend.IsStatus(err, 404) {
			return err
		}
		if t.spec.Image == "" {
			continue
		} // Prepare the shared base once first.
		probe := p.probesSources() && !fallback
		if probe {
			t.spec.Image = t.spec.Name
		}
		s := &SlotRecord{ID: id, Name: t.spec.Name, NativeHash: p.Store.hash(t.spec), ConfigHash: t.hash, Phase: "dispatch", Worker: t.worker, Created: time.Now().UTC(), Probe: probe}
		p.Store.data.Slots[id] = s
		if err = p.Store.save(); err != nil {
			return err
		}
		v, err := p.Backend.Create(ctx, t.spec)
		if err == nil && p.matchesSlot(s, v) {
			s.UID = v.UID
			s.Phase = "preparing"
		}
		if err = p.Store.save(); err != nil {
			return err
		}
	}
	return nil
}

// chooseSlot only reads fresh owned templates. The caller durably reserves the
// chosen slot with the launch intent before sending the clone POST.
func (p *Provider) chooseSlot(ctx context.Context, l compute.Launch, spec backend.Spec, inventory []backend.VM) (backend.Spec, string, compute.Status) {
	targets, err := p.slotTargets()
	if err != nil {
		return spec, "", compute.Unavailable
	}
	ids := []string{}
	for id, t := range targets {
		if p.dockerGroup(id).DockerProfile != "" {
			continue
		}
		if baseMatches(p.base(t.image), l) && t.spec.CPU >= spec.CPU && t.spec.Memory >= spec.Memory {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return spec, "", compute.Unsupported
	}
	preference := make(map[string]int, len(p.Config.Bases))
	for i, base := range p.Config.Bases {
		preference[base.Name] = i
	}
	sort.Slice(ids, func(i, j int) bool {
		pa, pb := preference[targets[ids[i]].image], preference[targets[ids[j]].image]
		if pa != pb {
			return pa < pb
		}
		a, b := targets[ids[i]].spec, targets[ids[j]].spec
		if a.CPU != b.CPU {
			return a.CPU < b.CPU
		}
		if a.Memory != b.Memory {
			return a.Memory < b.Memory
		}
		return ids[i] < ids[j]
	})
	for _, id := range ids {
		s := p.Store.data.Slots[id]
		t := targets[id]
		if s == nil || s.Phase != "ready" || s.ConfigHash != t.hash || p.slotBusy(id) {
			continue
		}
		// A foreign/unresolved clone of this local image also makes its MAC unavailable.
		busy := false
		for _, v := range inventory {
			if v.Image == s.LocalName {
				busy = true
				break
			}
		}
		if busy {
			continue
		}
		v, err := p.Backend.Get(ctx, s.Name)
		if err != nil {
			return spec, "", compute.Unavailable
		}
		if !p.matchesSlot(s, v) || !suspended(v) || v.LocalName != s.LocalName || v.Worker != s.Worker {
			s.Phase = "quarantine"
			if p.Store.save() != nil {
				return spec, "", compute.Unavailable
			}
			continue
		}
		name := spec.Name
		spec = t.spec
		spec.Name = name
		spec.Image = s.LocalName
		spec.StartupScript.Env = map[string]string{"PULSE_LAUNCH_ID": l.LaunchID, "PULSE_JOBDB": l.Job.JobDB, "PULSE_JOBDB_INSTANCE_ID": l.Job.InstanceID, "PULSE_TENANT_ID": l.Job.TenantID, "PULSE_JOB_ID": l.Job.JobID}

		payload, _ := json.Marshal(envelope{1, l.LaunchID, workerProcess(l, p.base(t.image), spec.CPU, spec.Memory)})
		spec.StartupScript.Content = startup(payload)
		// Read identity from this clone's snapshot, not an old host-side cache.
		// Published replacements deliberately have fresh MAC and boot identities.
		guard := fmt.Sprintf(`
if ! sudo -n /opt/pulse-tart/bin/python3 - <<'PULSE_RESTORE'
import json,re,subprocess
saved=json.load(open('/var/db/pulse-tart-template/ready'))
def read(*args): return subprocess.check_output(args,text=True).strip()
assert read('/usr/sbin/sysctl','-n','kern.bootsessionuuid') == saved['boot_id']
assert re.search(r'\bether\s+([0-9a-f:]{17})',read('/sbin/ifconfig','en0')).group(1) == saved['mac']
assert int(read('/usr/sbin/sysctl','-n','hw.ncpu')) == %d
assert int(read('/usr/sbin/sysctl','-n','hw.memsize')) == %d
assert read('/usr/bin/sw_vers','-productVersion') == '%s'
PULSE_RESTORE
then
 echo PULSE_TEMPLATE_RESTORE_FAILED
 sudo -n /sbin/shutdown -h now
 exit 1
fi
`, spec.CPU, spec.Memory*(1<<20), p.base(t.image).OSVersion)
		spec.StartupScript.Content = strings.Replace(spec.StartupScript.Content, "umask 077", "umask 077"+guard, 1)
		return spec, id, compute.Accepted
	}
	return spec, "", compute.NoCapacity
}

// SlotStatus contains operational metadata only; credentials and launch payloads
// remain outside this API. Phase describes the last successful reconciliation.
type SlotStatus struct {
	ID       string `json:"id"`
	Phase    string `json:"phase"`
	Worker   string `json:"worker"`
	Template string `json:"template"`
	MAC      string `json:"mac,omitempty"` // Preparation-time observation; may be empty or stale after external replacement.
	Busy     bool   `json:"busy"`
}

func (p *Provider) Slots() ([]SlotStatus, error) {
	p.Store.mu.Lock()
	defer p.Store.mu.Unlock()
	if p.Store.broken {
		return nil, ErrState
	}
	statuses := []SlotStatus{}
	for _, s := range p.Store.data.Slots {
		statuses = append(statuses, SlotStatus{ID: s.ID, Phase: s.Phase, Worker: s.Worker, Template: s.Name, MAC: s.MAC, Busy: p.slotBusy(s.ID)})
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].ID < statuses[j].ID })
	return statuses, nil
}
