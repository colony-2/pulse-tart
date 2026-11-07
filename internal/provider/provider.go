package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"time"

	compute "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-tart/internal/backend"
	"github.com/colony-2/pulse-tart/internal/config"
)

var ErrBackend = errors.New("backend observation unavailable")

type Backend interface {
	Info(context.Context) error
	List(context.Context) ([]backend.VM, error)
	Create(context.Context, backend.Spec) (backend.VM, error)
	Get(context.Context, string) (backend.VM, error)
	Delete(context.Context, string) error
}
type Provider struct {
	Config     config.Settings
	Store      *Store
	Backend    Backend
	Setup      func(context.Context) ([]byte, error)
	BaseScript []byte
	Assets     fs.FS
}

func New(c config.Settings, b Backend) (*Provider, error) {
	if _, ok := b.(templateBackend); !ok {
		return nil, ErrBackend
	}
	s, e := OpenStore(c.StateDir, c.Scope())
	if e != nil {
		return nil, e
	}
	return Attach(c, b, s), nil
}
func Attach(c config.Settings, b Backend, s *Store) *Provider {
	return &Provider{Config: c, Store: s, Backend: b}
}
func (p *Provider) Close() error { return p.Store.Close() }
func (p *Provider) Check(ctx context.Context) error {
	if e := p.Store.mu.LockContext(ctx); e != nil {
		return e
	}
	broken := p.Store.broken
	p.Store.mu.Unlock()
	if broken {
		return ErrState
	}
	if p.Backend.Info(ctx) != nil {
		return ErrBackend
	}
	_, e := p.Backend.List(ctx)
	if e != nil {
		return ErrBackend
	}
	return nil
}
func reason(status compute.Status) string {
	switch status {
	case compute.Accepted:
		return ""
	case compute.Unknown:
		return "provisioning outcome uncertain"
	case compute.NoCapacity:
		return "provider reservation budget exhausted"
	case compute.Unsupported:
		return "requested semantics are unsupported"
	case compute.Rejected:
		return "invalid or conflicting launch"
	default:
		return "provider unavailable before dispatch"
	}
}
func result(id string, status compute.Status) compute.Submission {
	return compute.Submission{LaunchID: id, Status: status, Reason: reason(status)}
}
func (p *Provider) Submit(ctx context.Context, ls []compute.Launch) ([]compute.Submission, error) {
	if len(ls) < 1 || len(ls) > 100 {
		return nil, errors.New("invalid batch size")
	}
	seen := map[string]bool{}
	for _, l := range ls {
		if seen[l.LaunchID] {
			return nil, errors.New("duplicate launch ID")
		}
		seen[l.LaunchID] = true
	}
	// Serialized native dispatch bounds concurrency and makes reservation checks atomic.
	// The durable intent is saved before every native operation.
	if e := p.Store.mu.LockContext(ctx); e != nil {
		return nil, e
	}
	defer p.Store.mu.Unlock()
	out := make([]compute.Submission, 0, len(ls))
	for _, l := range ls {
		if p.Store.broken {
			status := compute.Unavailable
			if p.Store.data.Records[l.LaunchID] != nil {
				status = compute.Unknown
			}
			out = append(out, result(l.LaunchID, status))
			continue
		}
		if !validLaunch(l) {
			out = append(out, result(l.LaunchID, compute.Rejected))
			continue
		}
		l = copyLaunch(l)
		hash := p.launchHash(l)
		if old := p.Store.data.Records[l.LaunchID]; old != nil {
			status := old.Outcome
			if old.Fingerprint != hash {
				status = compute.Rejected
			}
			out = append(out, result(l.LaunchID, status))
			continue
		}
		if l.StartBefore != nil && !time.Now().Before(*l.StartBefore) {
			out = append(out, result(l.LaunchID, compute.Rejected))
			continue
		}
		if ctx.Err() != nil || p.Store.broken {
			out = append(out, result(l.LaunchID, compute.Unavailable))
			continue
		}
		if submission, handled := p.submitDocker(ctx, l, hash); handled {
			out = append(out, submission)
			continue
		}
		spec, status := p.plan(l)
		if status != compute.Accepted {
			out = append(out, result(l.LaunchID, status))
			continue
		}
		active := 0
		for _, r := range p.Store.data.Records {
			if r.Phase != "done" {
				active++
			}
		}
		if active >= p.Config.Capacity() {
			out = append(out, result(l.LaunchID, compute.NoCapacity))
			continue
		}
		// A fresh read failure is fallback-safe because no intent was dispatched.
		inventory, e := p.Backend.List(ctx)
		if e != nil {
			out = append(out, result(l.LaunchID, compute.Unavailable))
			continue
		}
		if ctx.Err() != nil {
			out = append(out, result(l.LaunchID, compute.Unavailable))
			continue
		}
		slotID := ""
		if len(p.Config.Slots) > 0 {
			spec, slotID, status = p.chooseSlot(ctx, l, spec, inventory)
			if status != compute.Accepted {
				out = append(out, result(l.LaunchID, status))
				continue
			}
		}
		r := &Record{LaunchID: l.LaunchID, Fingerprint: hash, NativeHash: p.Store.hash(spec), Name: spec.Name, Job: l.Job, Created: time.Now().UTC(), Outcome: compute.Unknown, Phase: "dispatch", SlotID: slotID}
		p.Store.data.Records[l.LaunchID] = r
		if p.Store.save() != nil {
			out = append(out, result(l.LaunchID, compute.Unknown))
			continue
		}
		v, e := p.Backend.Create(ctx, spec)
		if e == nil && v.UID != "" && v.Name == r.Name && p.Store.hash(normalized(v.Spec)) == r.NativeHash {
			r.UID = v.UID
			r.Outcome = compute.Accepted
			r.Phase = "active"
		}
		// Even a non-2xx response can be generated by an intermediary after dispatch.
		// Reconcile by unique name; never blindly retry POST.
		if p.Store.save() != nil {
			out = append(out, result(l.LaunchID, compute.Unknown))
			continue
		}
		out = append(out, result(l.LaunchID, r.Outcome))
	}
	return out, nil
}
func (p *Provider) plan(l compute.Launch) (backend.Spec, compute.Status) {
	if l.TimeoutSeconds != 0 || l.StartBefore != nil || l.ScratchBytes != 0 {
		return backend.Spec{}, compute.Unsupported
	}
	cpu := (l.CPUMillis + 999) / 1000
	memory := (l.MemoryBytes + (1 << 20) - 1) / (1 << 20)
	for _, g := range p.Config.Slots {
		if g.DockerProfile != "" {
			continue
		}
		b := p.base(g.Base)
		if baseMatches(b, l) && g.CPU >= cpu && g.MemoryMiB >= memory {
			return backend.Spec{Name: "pulse-" + p.Config.Scope()[:12] + "-" + newID(), CPU: cpu, Memory: memory}, compute.Accepted
		}
	}
	return backend.Spec{}, compute.Unsupported
}
func startup(envelope []byte) string {
	return fmt.Sprintf(`#!/bin/bash
set -eu
set +x
umask 077
work=$(mktemp -d /tmp/pulse-bootstrap.XXXXXX)
trap 'rm -rf "$work"' EXIT
# Tools already live in the suspended image. Never reinstall them on a job.
sudo -n /bin/bash /opt/pulse-tart-image/bootstrap-guest.sh
/usr/bin/base64 -D > "$work/envelope.json" <<'PULSE_ENVELOPE'
%s
PULSE_ENVELOPE
sudo -n /opt/pulse-tart/bin/pulse-tart-guest ingest < "$work/envelope.json"
echo PULSE_HANDOFF_OK
while ! sudo -n test -f /var/db/pulse-tart/complete; do sleep 1; done
if [ "$(sudo -n cat /var/db/pulse-tart/complete)" = 0 ]; then echo PULSE_WORKER_OK; else echo PULSE_WORKER_FAILED; fi
`, base64.StdEncoding.EncodeToString(envelope))
}
func normalized(v backend.Spec) backend.Spec {
	if v.Labels == nil {
		v.Labels = map[string]string{}
	}
	if v.StartupScript.Env == nil {
		v.StartupScript.Env = map[string]string{}
	}
	return v
}
func (p *Provider) matches(r *Record, v backend.VM) bool {
	return r.Name == v.Name && (r.UID == "" || r.UID == v.UID) && v.UID != "" && p.Store.hash(normalized(v.Spec)) == r.NativeHash
}
func (p *Provider) Reconcile(ctx context.Context) error {
	if e := p.Store.mu.LockContext(ctx); e != nil {
		return e
	}
	defer p.Store.mu.Unlock()
	if p.Store.broken {
		return ErrState
	}
	vms, e := p.Backend.List(ctx)
	if e != nil {
		return ErrBackend
	}
	byName := map[string]backend.VM{}
	for _, v := range vms {
		byName[v.Name] = v
	}
	for _, r := range p.Store.data.Records {
		if r.PairID != "" {
			continue
		}
		if r.Phase == "done" {
			continue
		}
		v, ok := byName[r.Name]
		if !ok {
			if r.Outcome == compute.Accepted {
				r.Phase = "done"
			}
			continue
		}
		if !p.matches(r, v) {
			r.Phase = "quarantine"
			continue
		}
		r.UID = v.UID
		r.Outcome = compute.Accepted
		if (v.Status == "failed" && v.Stopped()) || r.Phase == "deleting" {
			r.Phase = "deleting"
			if e = p.Store.save(); e != nil {
				return e
			}
			// Recheck identity immediately before deletion; the scope requires exclusive name mutation.
			current, e := p.Backend.Get(ctx, r.Name)
			if backend.IsStatus(e, 404) {
				r.Phase = "done"
				continue
			}
			if e != nil {
				return ErrBackend
			}
			if !p.matches(r, current) {
				r.Phase = "quarantine"
				continue
			}
			if !current.Stopped() {
				continue
			}
			if e = p.Backend.Delete(ctx, r.Name); e != nil && !backend.IsStatus(e, 404) {
				return ErrBackend
			}
			_, e = p.Backend.Get(ctx, r.Name)
			if backend.IsStatus(e, 404) {
				r.Phase = "done"
			} else if e != nil {
				return ErrBackend
			}
		} else {
			r.Phase = "active"
		}
	}
	if e = p.Store.save(); e != nil {
		return e
	}
	if e := p.reconcileImages(ctx); e != nil {
		return e
	}
	if e := p.reconcileSlots(ctx); e != nil {
		return e
	}
	return p.reconcilePairs(ctx)
}
func (p *Provider) List(ctx context.Context, q compute.ListRequest) (compute.ListResponse, error) {
	if q.PageSize == 0 {
		q.PageSize = 100
	}
	if q.PageSize < 1 || q.PageSize > 100 || len(q.PageToken) > 16384 {
		return compute.ListResponse{}, errors.New("invalid list request")
	}
	if e := p.Store.mu.LockContext(ctx); e != nil {
		return compute.ListResponse{}, e
	}
	defer p.Store.mu.Unlock()
	if p.Store.broken {
		return compute.ListResponse{}, ErrState
	}
	after := ""
	if q.PageToken != "" {
		var c struct{ Scope, Launch, After, MAC string }
		b, e := base64.RawURLEncoding.DecodeString(q.PageToken)
		if e != nil || json.Unmarshal(b, &c) != nil || c.Scope != p.Config.Scope() || c.Launch != q.LaunchID || c.MAC != p.Store.hash([]string{c.Scope, c.Launch, c.After}) {
			return compute.ListResponse{}, errors.New("invalid cursor")
		}
		after = c.After
	}
	vs, e := p.Backend.List(ctx)
	if e != nil {
		return compute.ListResponse{}, ErrBackend
	}
	records := map[string]*Record{}
	for _, r := range p.Store.data.Records {
		if r.PairID == "" {
			records[r.Name] = r
		}
	}
	items, err := p.pairInstances(vs, q, after)
	if err != nil {
		return compute.ListResponse{}, err
	}
	for _, v := range vs {
		r := records[v.Name]
		if r == nil || r.Phase == "done" {
			continue
		}
		if !p.matches(r, v) {
			return compute.ListResponse{}, errors.New("owned instance identity mismatch")
		}
		if v.Status == "failed" && v.Stopped() {
			continue
		}
		state := "starting"
		if v.Status == "pending" {
			state = "queued"
		}
		if v.Status == "running" || (v.Status == "failed" && !v.Stopped()) {
			state = "running"
		}
		if v.PowerState == "suspended" {
			state = "paused"
		}
		if r.Phase == "deleting" {
			state = "stopping"
		}
		if r.Name <= after || (q.LaunchID != "" && r.LaunchID != q.LaunchID) {
			continue
		}
		created := r.Created
		item := compute.Instance{ID: r.Name, LaunchID: r.LaunchID, State: state, Job: r.Job, Refs: []string{p.Config.Kind + ":" + r.Name}, CreatedAt: &created}
		if !v.StartedAt.IsZero() {
			t := v.StartedAt
			item.StartedAt = &t
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	out := compute.ListResponse{Items: items}
	if len(items) > q.PageSize {
		out.Items = items[:q.PageSize]
		after = out.Items[len(out.Items)-1].ID
		scope := p.Config.Scope()
		c := struct{ Scope, Launch, After, MAC string }{scope, q.LaunchID, after, p.Store.hash([]string{scope, q.LaunchID, after})}
		b, _ := json.Marshal(c)
		out.NextPageToken = base64.RawURLEncoding.EncodeToString(b)
	}
	return out, nil
}
