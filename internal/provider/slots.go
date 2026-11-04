package provider

import (
	"context"
	"encoding/base64"
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
}

type slotTarget struct {
	spec   backend.Spec
	image  string
	worker string
	hash   string
}

func validSlotPhase(s string) bool {
	switch s {
	case "dispatch", "preparing", "suspending", "ready", "deleting", "quarantine", "failed":
		return true
	}
	return false
}

func (p *Provider) slotTargets() (map[string]slotTarget, error) {
	targets := map[string]slotTarget{}
	for _, group := range p.Config.Slots {
		img := p.base(group.Base)
		labels := map[string]string{}
		for k, v := range p.Config.WorkerLabels {
			labels[k] = v
		}
		labels[config.WorkerNameLabel] = p.Config.Worker
		spec := backend.Spec{OS: "darwin", Arch: "arm64", Runtime: "tart", Image: img.Image, CPU: group.CPU, Memory: group.MemoryMiB, Headless: true, Username: img.SSHUsername, Password: string(img.SSHPassword), RestartPolicy: "Never", Suspendable: true, Labels: labels, Resources: map[string]int64{"org.cirruslabs.logical-cores": group.CPU, "org.cirruslabs.memory-mib": group.MemoryMiB, "org.cirruslabs.tart-vms": 1}, StartupScript: backend.Script{Content: templateStartup(p.bundle, int(p.Config.BootstrapTimeout.Seconds()), img), Env: map[string]string{}}}
		target := slotTarget{spec: spec, image: group.Base, worker: p.Config.Worker, hash: p.Store.hash(struct {
			Base    string
			Spec    backend.Spec
			HostKey []byte
		}{group.Base, spec, img.SSHHostKey})}
		for i := 0; i < group.Count; i++ {
			targets[fmt.Sprintf("%s-%04d", group.Name, i+1)] = target
		}
	}
	return targets, nil
}

func templateStartup(bundle []byte, seconds int, base config.Base) string {
	return fmt.Sprintf(`#!/bin/bash
set -eu
set +x
umask 077
work=$(mktemp -d /tmp/pulse-template.XXXXXX)
trap 'rm -rf "$work"' EXIT
/usr/bin/base64 -D > "$work/bundle.tar.gz" <<'PULSE_BUNDLE'
%s
PULSE_BUNDLE
/usr/bin/tar -xzf "$work/bundle.tar.gz" -C "$work"
sudo -n /bin/bash "$work/prepare-template.sh" "$work" %d %s %s
`, base64.StdEncoding.EncodeToString(bundle), seconds, base.OSVersion, "'"+strings.Join(base.Capabilities, " ")+"'")
}

func (p *Provider) slotBusy(id string) bool {
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
	return s.Phase != "ready" || (s.UID != "" && s.LocalName != "" && s.MAC != "" && s.BootID != "")
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
			// Unused builders contain no job. Suspend them before retiring a changed config.
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
			s.LocalName = v.LocalName
			s.Phase = "ready"
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
		if p.Store.data.Slots[id] != nil || p.slotBusy(id) {
			continue
		}
		t := targets[id]
		t.spec.Name = "pulse-template-" + p.Config.Scope()[:12] + "-" + id + "-" + newID()
		s := &SlotRecord{ID: id, Name: t.spec.Name, NativeHash: p.Store.hash(t.spec), ConfigHash: t.hash, Phase: "dispatch", Worker: t.worker, Created: time.Now().UTC()}
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
		if baseMatches(p.base(t.image), l) && t.spec.CPU >= spec.CPU && t.spec.Memory >= spec.Memory {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return spec, "", compute.Unsupported
	}
	sort.Slice(ids, func(i, j int) bool {
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
		spec.StartupScript.Content = startup(p.bundle, payload, int(p.Config.BootstrapTimeout.Seconds()))
		// Prevent accidental cold boots (including Tart's MAC-collision fallback)
		// from executing a lease. Only a true restore of this slot may accept work.
		guard := fmt.Sprintf(`
if [ "$(/usr/sbin/sysctl -n kern.bootsessionuuid)" != '%s' ] || [ "$(/sbin/ifconfig en0 | /usr/bin/awk '/ether / {print $2; exit}')" != '%s' ]; then
  echo PULSE_TEMPLATE_RESTORE_FAILED
  sudo -n /sbin/shutdown -h now
  exit 1
fi
`, s.BootID, s.MAC)
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
	MAC      string `json:"mac,omitempty"`
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
