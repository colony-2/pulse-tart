// Package tart provides macOS ARM64 concurrency slots through Orchard or local
// Tart. It implements the Pulse provider API as an in-process Go library.
package tart

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-tart/internal/config"
	"github.com/colony-2/pulse-tart/internal/orchard"
	engine "github.com/colony-2/pulse-tart/internal/provider"
	local "github.com/colony-2/pulse-tart/internal/tart"
)

type Config = config.Config
type Base = config.Base
type SlotGroup = config.SlotGroup
type OrchardConfig = config.Orchard
type TartConfig = config.Tart
type Secret = config.Secret
type SlotStatus = engine.SlotStatus

const DefaultCPU = config.DefaultCPU
const DefaultMemoryMiB = config.DefaultMemoryMiB

var ErrConfig = config.Err
var ErrClosed = errors.New("provider closed")
var _ api.Provider = (*Runtime)(nil)

// Runtime owns one state directory exclusively. Run or Reconcile prepares slots
// and cleans up completed instances. Submit and List implement the shared API.
// Close releases local resources; running VMs survive it. A new Runtime using
// the same state directory recovers the journal. Never share a directory across
// backends, owners, or simultaneously running hosts.
type Runtime struct {
	mu     sync.RWMutex
	p      *engine.Provider
	closed bool
	period time.Duration
}

func prepare(c Config) (Config, error) {
	c, e := config.Normalize(c)
	if e != nil {
		return c, e
	}
	if len(c.GuestBundle) == 0 {
		c.GuestBundle, e = GuestBundle()
	}
	return c, e
}
func NewOrchard(c Config, connection OrchardConfig) (*Runtime, error) {
	c, e := prepare(c)
	if e != nil {
		return nil, e
	}
	if !config.Name(connection.Worker) {
		return nil, ErrConfig
	}
	connection.WorkerLabels = maps.Clone(connection.WorkerLabels)
	connection.CACert = slices.Clone(connection.CACert)
	client, e := orchard.New(connection)
	if e != nil {
		return nil, e
	}
	settings := config.Settings{Config: c, Kind: "orchard", Endpoint: client.Base, Worker: connection.Worker, WorkerLabels: connection.WorkerLabels}
	p, e := engine.New(settings, client)
	if e != nil {
		return nil, e
	}
	return &Runtime{p: p, period: c.PollInterval}, nil
}
func NewTart(c Config, options TartConfig) (*Runtime, error) {
	c, e := prepare(c)
	if e != nil {
		return nil, e
	}
	if options.Home == "" {
		home, e := os.UserHomeDir()
		if e != nil {
			return nil, e
		}
		options.Home = filepath.Join(home, ".tart")
	}
	if !filepath.IsAbs(options.Home) {
		return nil, ErrConfig
	}
	options.Home = filepath.Clean(options.Home)
	settings := config.Settings{Config: c, Kind: "tart", Endpoint: options.Home, Worker: "local"}
	store, e := engine.OpenStore(c.StateDir, settings.Scope())
	if e != nil {
		return nil, e
	}
	b, e := local.New(options, c)
	if e != nil {
		store.Close()
		return nil, e
	}
	return &Runtime{p: engine.Attach(settings, b, store), period: c.PollInterval}, nil
}
func (r *Runtime) Submit(ctx context.Context, launches []api.Launch) ([]api.Submission, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrClosed
	}
	return r.p.Submit(ctx, launches)
}
func (r *Runtime) List(ctx context.Context, q api.ListRequest) (api.ListResponse, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return api.ListResponse{}, ErrClosed
	}
	return r.p.List(ctx, q)
}
func (r *Runtime) Check(ctx context.Context) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return ErrClosed
	}
	return r.p.Check(ctx)
}
func (r *Runtime) Slots() ([]SlotStatus, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrClosed
	}
	return r.p.Slots()
}
func (r *Runtime) Reconcile(ctx context.Context) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return ErrClosed
	}
	if b, ok := r.p.Backend.(interface{ Recover(context.Context) error }); ok {
		if e := b.Recover(ctx); e != nil {
			return e
		}
	}
	return r.p.Reconcile(ctx)
}

// Run reconciles immediately, then every PollInterval. It returns the first
// observation error so the host can log it and decide its own retry policy.
// Cancellation stops this loop, not running VM jobs.
func (r *Runtime) Run(ctx context.Context) error {
	for {
		if e := r.Reconcile(ctx); e != nil {
			return e
		}
		timer := time.NewTimer(r.period)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Drain disables new admissions and retires templates as their jobs finish.
// Call Reconcile until Slots is empty. It does not cancel active work.
func (r *Runtime) Drain(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	for i := range r.p.Config.Slots {
		r.p.Config.Slots[i].Count = 0
	}
	return r.p.Reconcile(ctx)
}
func (r *Runtime) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if b, ok := r.p.Backend.(interface{ Close() error }); ok {
		_ = b.Close()
	}
	return r.p.Close()
}
