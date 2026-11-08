package provider

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/colony-2/pulse-tart/internal/backend"
	"github.com/colony-2/pulse-tart/internal/config"
)

type ImageRecord struct {
	Name       string    `json:"name"`
	UID        string    `json:"uid"`
	NativeHash string    `json:"native_hash"`
	Phase      string    `json:"phase"`
	LocalName  string    `json:"local_name"`
	Worker     string    `json:"worker"`
	Probe      bool      `json:"probe,omitempty"`
	Kind       string    `json:"kind"`
	OS         string    `json:"os,omitempty"`
	Created    time.Time `json:"created"`
}

type ImageStatus struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Phase     string `json:"phase"`
	Worker    string `json:"worker"`
	LocalName string `json:"local_name,omitempty"`
}

func (p *Provider) Images() ([]ImageStatus, error) {
	p.Store.mu.Lock()
	defer p.Store.mu.Unlock()
	if p.Store.broken {
		return nil, ErrState
	}
	out := []ImageStatus{}
	for _, r := range p.Store.data.Images {
		out = append(out, ImageStatus{UID: r.UID, Name: r.Name, Kind: r.Kind, Phase: r.Phase, Worker: r.Worker, LocalName: r.LocalName})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (p *Provider) probesSources() bool {
	if b, ok := p.Backend.(interface{ ProbeLocalSources() bool }); ok {
		return b.ProbeLocalSources()
	}
	return false
}

func (p *Provider) sourceSpec(b config.Base, cpu, memory int64) backend.Spec {
	labels := map[string]string{}
	for k, v := range p.Config.WorkerLabels {
		labels[k] = v
	}
	labels[config.WorkerNameLabel] = p.Config.Worker
	osname := b.OS
	if osname == "" {
		osname = "darwin"
	}
	// Orchard's Tart slot resource limits macOS instances, not Linux companions.
	macSlots := int64(1)
	if osname == "linux" {
		macSlots = 0
	}
	return backend.Spec{OS: osname, Arch: "arm64", Runtime: "tart", Image: b.Image, CPU: cpu, Memory: memory, Headless: true, Username: b.SSHUsername, Password: string(b.SSHPassword), RestartPolicy: "Never", Suspendable: osname == "darwin", Labels: labels, Resources: map[string]int64{"org.cirruslabs.logical-cores": cpu, "org.cirruslabs.memory-mib": memory, "org.cirruslabs.tart-vms": macSlots}, StartupScript: backend.Script{Env: map[string]string{}}}
}
func (p *Provider) retain(ctx context.Context, name, uid string) error {
	if b, ok := p.Backend.(interface {
		RetainSource(context.Context, string, string) error
	}); ok {
		return b.RetainSource(ctx, name, uid)
	}
	return nil
}
func (p *Provider) adopt(ctx context.Context, spec backend.Spec, base config.Base) (backend.VM, error) {
	if b, ok := p.Backend.(interface {
		AdoptSource(context.Context, backend.Spec, []byte) (backend.VM, error)
	}); ok {
		return b.AdoptSource(ctx, spec, base.SSHHostKey)
	}
	return p.Backend.Get(ctx, spec.Name)
}
func sourceStopped(v backend.VM) bool {
	// Orchard powerState is desired state and can remain "running" after a
	// clean guest shutdown. Its running condition is the observed state.
	return v.Stopped() && v.PowerState != "suspended" && v.LocalName != ""
}
func (p *Provider) imageMatches(r *ImageRecord, v backend.VM) bool {
	return v.Name == r.Name && v.UID != "" && (r.UID == "" || r.UID == v.UID) && p.Store.hash(normalized(v.Spec)) == r.NativeHash && (v.Worker == "" || v.Worker == r.Worker)
}
func eventHas(events []backend.Event, marker string) bool {
	for _, event := range events {
		for _, line := range strings.Split(event.Payload+"\n"+event.Message, "\n") {
			if strings.TrimSpace(line) == marker {
				return true
			}
		}
	}
	return false
}
func baseStartup(setup, script []byte, seconds int, osVersion string, reuse bool) string {
	if reuse {
		return fmt.Sprintf("#!/bin/bash\nset -eu\nwork=$(mktemp -d)\ntrap 'rm -rf \"$work\"' EXIT\n/usr/bin/base64 -D > \"$work/prepare-base.sh\" <<'PULSE_SCRIPT'\n%s\nPULSE_SCRIPT\nsudo -n /bin/bash \"$work/prepare-base.sh\" \"$work\" %d %s reuse\n", base64.StdEncoding.EncodeToString(script), seconds, osVersion)
	}
	return fmt.Sprintf("#!/bin/bash\nset -eu\nwork=$(mktemp -d)\ntrap 'rm -rf \"$work\"' EXIT\n/usr/bin/base64 -D > \"$work/setup.tar.gz\" <<'PULSE_SETUP'\n%s\nPULSE_SETUP\n/usr/bin/tar -xzf \"$work/setup.tar.gz\" -C \"$work\"\nsudo -n /bin/bash \"$work/prepare-base.sh\" \"$work\" %d %s\n", base64.StdEncoding.EncodeToString(setup), seconds, osVersion)
}
func (p *Provider) reconcileImages(ctx context.Context) error {
	wanted := map[string]config.Base{}
	for _, g := range p.Config.Slots {
		if g.Count > 0 {
			b := p.base(g.Base)
			wanted[p.baseImageName(b.Name)] = b
		}
	}
	names := []string{}
	for name := range wanted {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		base := wanted[name]
		r := p.Store.data.Images[name]
		if r != nil && r.Phase != "deleting" {
			if r.Phase == "quarantine" || r.Phase == "failed" {
				continue
			}
			v, err := p.Backend.Get(ctx, name)
			if backend.IsStatus(err, 404) {
				if r.UID != "" {
					delete(p.Store.data.Images, name)
				}
				continue
			}
			if err != nil {
				return ErrBackend
			}
			if !p.imageMatches(r, v) {
				r.Phase = "quarantine"
				continue
			}
			r.UID = v.UID
			if r.Phase == "ready" {
				if !sourceStopped(v) {
					r.Phase = "quarantine"
				}
				continue
			}
			events, err := p.Backend.(templateBackend).Events(ctx, name)
			if err != nil {
				return ErrBackend
			}
			if eventHas(events, "PULSE_BASE_READY") && sourceStopped(v) {
				if err := p.retain(ctx, name, v.UID); err != nil {
					return err
				}
				r.Phase = "ready"
				r.LocalName = v.LocalName
				continue
			}
			if v.Status == "failed" && v.Stopped() {
				if r.Probe {
					// The worker-local alias was absent or unusable. The exact observed
					// probe is disposable and has never contained a lease.
					if err := p.Backend.Delete(ctx, name); err != nil {
						return err
					}
					r.Probe = false
					r.UID = ""
					r.Phase = "deleting"
				} else {
					r.Phase = "failed"
				}
			}
			if r.Phase != "deleting" {
				continue
			}
		}
		spec := p.sourceSpec(base, config.DefaultCPU, config.DefaultMemoryMiB)
		spec.Name = name
		probe := p.probesSources() && r == nil && base.OS != "linux"
		if r == nil {
			existing, err := p.adopt(ctx, spec, base)
			if err == nil {
				if !sourceStopped(existing) || existing.Worker != p.Config.Worker || existing.CPU != spec.CPU || existing.Memory != spec.Memory {
					return fmt.Errorf("prepared base %s is not stopped or has incompatible hardware", name)
				}
				if err := p.retain(ctx, name, existing.UID); err != nil {
					return err
				}
				p.Store.data.Images[name] = &ImageRecord{Name: name, UID: existing.UID, NativeHash: p.Store.hash(normalized(existing.Spec)), Phase: "ready", LocalName: existing.LocalName, Worker: p.Config.Worker, Kind: "base", OS: base.OS, Created: time.Now().UTC()}
				continue
			}
			if !backend.IsStatus(err, 404) {
				return err
			}
		}
		available, err := p.canStartVMs(ctx, spec)
		if err != nil {
			return err
		}
		if !available {
			continue
		}
		var setup []byte
		if probe {
			spec.Image = name
		} else {
			if p.Setup == nil {
				return fmt.Errorf("image installer is not configured")
			}
			var err error
			setup, err = p.Setup(ctx)
			if err != nil {
				return err
			}
		}
		spec.StartupScript.Content = baseStartup(setup, p.BaseScript, int(p.Config.BootstrapTimeout.Seconds()), base.OSVersion, probe)
		if base.OS == "linux" {
			spec.StartupScript.Content = strings.ReplaceAll(spec.StartupScript.Content, "/usr/bin/base64 -D", "/usr/bin/base64 -d")
			spec.StartupScript.Content = strings.ReplaceAll(spec.StartupScript.Content, "prepare-base.sh", "prepare-base-linux.sh")
		}
		r = &ImageRecord{Name: name, Phase: "dispatch", Probe: probe, Kind: "base", OS: base.OS, NativeHash: p.Store.hash(spec), Worker: p.Config.Worker, Created: time.Now().UTC()}
		p.Store.data.Images[name] = r
		if err := p.Store.save(); err != nil {
			return err
		}
		v, err := p.Backend.Create(ctx, spec)
		if err == nil && p.imageMatches(r, v) {
			r.UID = v.UID
			r.Phase = "preparing"
		}
		if err := p.Store.save(); err != nil {
			return err
		}
	}
	return p.Store.save()
}

// PurgeImages is an explicit maintenance operation, never normal reconciliation.
// Call after Drain and after all jobs have completed. Published images otherwise
// survive close, drain, and updates to the library's setup code.
func (p *Provider) PurgeImages(ctx context.Context) error {
	if err := p.Store.mu.LockContext(ctx); err != nil {
		return err
	}
	defer p.Store.mu.Unlock()
	for _, pair := range p.Store.data.Pairs {
		if pair.Phase != "gone" {
			return fmt.Errorf("Docker allocations are still owned")
		}
	}
	if p.Config.Capacity() != 0 || len(p.Store.data.Slots) != 0 {
		return fmt.Errorf("drain slots before purging images")
	}
	for _, r := range p.Store.data.Records {
		if r.Phase != "done" {
			return fmt.Errorf("jobs are still active")
		}
	}
	for name, r := range p.Store.data.Images {
		v, err := p.Backend.Get(ctx, name)
		if backend.IsStatus(err, 404) {
			delete(p.Store.data.Images, name)
			continue
		}
		if err != nil {
			return err
		}
		if !p.imageMatches(r, v) || !v.Stopped() {
			return fmt.Errorf("image %s is not safely removable", name)
		}
		if err := p.Backend.Delete(ctx, name); err != nil {
			return err
		}
		delete(p.Store.data.Images, name)
	}
	return p.Store.save()
}

func (p *Provider) baseImageName(name string) string {
	for _, g := range p.Config.Slots {
		if g.Base == name && g.DockerProfile != "" {
			return config.BaseImageName(p.Config.OwnerID, name+"-docker")
		}
	}
	return config.BaseImageName(p.Config.OwnerID, name)
}
