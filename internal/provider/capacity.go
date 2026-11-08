package provider

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"time"

	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-tart/internal/backend"
	"github.com/colony-2/pulse-tart/internal/config"
)

// vmReservations includes native inventory and durable intents that may not yet
// appear there. It protects the hard two-macOS limit, including builders and
// foreign VMs. Called under the journal lock before recording a dispatch intent.
func (p *Provider) vmReservations(ctx context.Context) (map[string]string, error) {
	vms, err := p.Backend.List(ctx)
	if err != nil {
		return nil, ErrBackend
	}
	used := map[string]string{}
	observed := map[string]backend.VM{}
	for _, v := range vms {
		if p.Config.Kind == "orchard" && v.Worker != p.Config.Worker && v.Labels[config.WorkerNameLabel] != p.Config.Worker && (v.Worker != "" || v.Labels[config.WorkerNameLabel] != "") {
			continue
		}
		observed[v.Name] = v
		if !v.Stopped() || v.Status == "pending" {
			used[v.Name] = v.OS
		}
	}
	for _, r := range p.Store.data.Records {
		if r.Phase != "done" && r.PairID == "" {
			used[r.Name] = "darwin"
		}
	}
	for _, pair := range p.Store.data.Pairs {
		if pair.Phase != "gone" {
			used[pair.Primary.Name] = pair.Primary.OS
			if pair.Engine.Name != "" {
				used[pair.Engine.Name] = "linux"
			}
		}
	}
	reserveSource := func(name, phase, os string) {
		if phase == "ready" || phase == "fallback" {
			return
		}
		if v, ok := observed[name]; ok && v.Stopped() && v.Status != "pending" {
			return
		}
		used[name] = os
	}
	for _, r := range p.Store.data.Images {
		os := r.OS
		if os == "" {
			// Compatibility with journals written before source OS was recorded.
			for _, b := range p.Config.Bases {
				if p.baseImageName(b.Name) == r.Name {
					os = b.OS
				}
			}
		}
		reserveSource(r.Name, r.Phase, os)
	}
	for _, s := range p.Store.data.Slots {
		reserveSource(s.Name, s.Phase, "darwin")
	}
	return used, nil
}

func (p *Provider) jobReservations() map[string]string {
	used := map[string]string{}
	for _, r := range p.Store.data.Records {
		if r.Phase != "done" && r.PairID == "" {
			used[r.Name] = "darwin"
		}
	}
	for _, pair := range p.Store.data.Pairs {
		if pair.Phase != "gone" {
			used[pair.Primary.Name] = pair.Primary.OS
		}
	}
	return used
}

func (p *Provider) fitsJobCounts(used map[string]string, specs ...backend.Spec) bool {
	for _, spec := range specs {
		if spec.Name != "" {
			used[spec.Name] = spec.OS
		}
	}
	mac, linux := 0, 0
	for _, os := range used {
		if os == "linux" {
			linux++
		} else {
			mac++
		}
	}
	limits, err := p.Config.VMCounts.Normalize()
	return err == nil && mac <= *limits.MacOS && linux <= *limits.Linux && mac+linux <= *limits.Total
}

func (p *Provider) canStartVMs(ctx context.Context, specs ...backend.Spec) (bool, error) {
	used, err := p.vmReservations(ctx)
	if err != nil {
		return false, err
	}
	for _, spec := range specs {
		if spec.Name != "" {
			used[spec.Name] = spec.OS
		}
	}
	return macOSCount(used) <= 2, nil
}

func macOSCount(used map[string]string) int {
	mac := 0
	for _, os := range used {
		// Unknown foreign VM OS must never bypass the two-macOS ceiling.
		if os != "linux" {
			mac++
		}
	}
	return mac
}

// A shared total can be smaller than the sum of the OS counts. In that case an
// idle warm pool must not permanently prevent another OS from accepting jobs.
// Reclaim only an unused ready allocation; never retain the refused launch or
// lease. Pulse must retry after reconciliation has prepared the requested pool.
func (p *Provider) requestJobRoom(ctx context.Context, l api.Launch) error {
	for _, base := range p.Config.Bases {
		for _, g := range p.Config.Slots {
			if g.Base != base.Name || g.Count == 0 || !baseMatches(base, l) && !p.dockerMatch(g, l) {
				continue
			}
			if g.DockerProfile != "" {
				if !p.dockerMatch(g, l) {
					continue
				}
			} else if slices.Contains(l.Capabilities, "docker") || g.CPU*1000 < l.CPUMillis || g.MemoryMiB*(1<<20) < l.MemoryBytes {
				continue
			}
			free := false
			for i := 1; i <= g.Count; i++ {
				if !p.slotBusy(fmt.Sprintf("%s-%04d", g.Name, i)) {
					free = true
					break
				}
			}
			if !free {
				continue
			}
			spec := p.sourceSpec(base, g.CPU, g.MemoryMiB)
			spec.Name = "capacity-request" // Used only for arithmetic, never dispatched.
			if !p.fitsJobCounts(map[string]string{}, spec) {
				continue
			}
			p.preferredGroup, p.preferUntil = g.Name, time.Now().Add(p.dockerReadyTimeout())
			used, err := p.vmReservations(ctx)
			if err != nil {
				return err
			}
			used[spec.Name] = spec.OS
			jobs := p.jobReservations()
			if p.fitsJobCounts(maps.Clone(jobs), spec) && macOSCount(used) <= 2 {
				return nil
			}
			ids := make([]string, 0, len(p.Store.data.Pairs))
			for id := range p.Store.data.Pairs {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				pair := p.Store.data.Pairs[id]
				if pair.Phase != "ready" || pair.LaunchID != "" || p.dockerGroup(pair.SlotID).Name == g.Name {
					continue
				}
				remaining, physical := maps.Clone(jobs), maps.Clone(used)
				delete(remaining, pair.Primary.Name)
				delete(physical, pair.Primary.Name)
				delete(physical, pair.Engine.Name)
				if p.fitsJobCounts(remaining, spec) && macOSCount(physical) <= 2 {
					pair.Phase = "reclaiming"
					return p.Store.save()
				}
			}
			return nil
		}
	}
	return nil
}

func (p *Provider) canReserveJobs(ctx context.Context, primaries ...backend.Spec) (bool, error) {
	if !p.fitsJobCounts(p.jobReservations(), primaries...) {
		return false, nil
	}
	return p.canStartVMs(ctx, primaries...)
}
