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
	"github.com/colony-2/pulse-tart/internal/bundle"
	"github.com/colony-2/pulse-tart/internal/config"
	"github.com/colony-2/pulse-tart/internal/orchard"
	engine "github.com/colony-2/pulse-tart/internal/provider"
	"github.com/colony-2/pulse-tart/internal/releases"
	local "github.com/colony-2/pulse-tart/internal/tart"
)

type Config = config.Config
type Base = config.Base
type SlotGroup = config.SlotGroup
type VMCounts = config.VMCounts
type OrchardConfig = config.Orchard
type TartConfig = config.Tart
type DockerConfig = config.Docker
type DockerEngineProfile = config.EngineProfile
type DockerEndpoint = config.Endpoint
type Secret = config.Secret
type SlotStatus = engine.SlotStatus
type ImageStatus = engine.ImageStatus

const DefaultCPU = config.DefaultCPU
const DefaultMemoryMiB = config.DefaultMemoryMiB
const DefaultMacOSVMCount = config.DefaultMacOSVMCount
const DefaultLinuxVMCount = config.DefaultLinuxVMCount

var ErrConfig = config.Err
var ErrClosed = errors.New("provider closed")
var _ api.Provider = (*Runtime)(nil)

// Runtime owns one state directory exclusively. Run or Reconcile prepares slots
// and cleans up completed instances. GetCapabilities, Submit and List implement
// the shared API.
// Close releases local resources; running VMs survive it. A new Runtime using
// the same state directory recovers the journal. Never share a directory across
// backends, owners, or simultaneously running hosts.
type Runtime struct {
	mu      sync.RWMutex
	p       *engine.Provider
	closed  bool
	period  time.Duration
	profile string
}

func prepare(c Config) (Config, error) {
	return ResolveConfig(c)
}

func newRuntime(p *engine.Provider, c Config) *Runtime {
	r := &Runtime{p: p, period: c.PollInterval, profile: "default"}
	if c.Docker != nil {
		r.profile = "docker"
	}
	p.Assets = assets
	p.BaseScript, _ = assets.ReadFile("scripts/prepare-base.sh")
	var setup []byte
	p.Setup = func(ctx context.Context) ([]byte, error) {
		if setup != nil {
			return setup, nil
		}
		revision, err := releases.Resolve(ctx, c.C2JVersion)
		if err != nil {
			return nil, err
		}
		setup, err = bundle.Build(assets, r.profile, revision)
		return setup, err
	}
	return r
}
func NewOrchard(c Config, connection OrchardConfig) (*Runtime, error) {
	c, e := prepare(c)
	if e != nil {
		return nil, e
	}
	if !config.Name(connection.Worker) {
		return nil, ErrConfig
	}
	if c.NeedsDockerEndpoint() {
		if e := connection.DockerEndpoint.Validate(); e != nil {
			return nil, e
		}
	}
	connection.WorkerLabels = maps.Clone(connection.WorkerLabels)
	connection.CACert = slices.Clone(connection.CACert)
	client, e := orchard.New(connection)
	if e != nil {
		return nil, e
	}
	client.GuestKeys = map[string][]byte{}
	for _, b := range c.Bases {
		client.GuestKeys[b.Image] = b.SSHHostKey
	}
	if c.Docker != nil {
		for _, p := range c.Docker.Profiles {
			if len(client.GuestKeys[p.Image]) == 0 {
				client.GuestKeys[p.Image] = p.SSHHostKey
			}
		}
	}
	settings := config.Settings{Config: c, Kind: "orchard", Endpoint: client.Base, Worker: connection.Worker, WorkerLabels: connection.WorkerLabels, DockerEndpoint: connection.DockerEndpoint}
	p, e := engine.New(settings, client)
	if e != nil {
		return nil, e
	}
	return newRuntime(p, c), nil
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
	if c.NeedsDockerEndpoint() {
		if e := options.DockerEndpoint.Validate(); e != nil {
			return nil, e
		}
	}
	settings := config.Settings{Config: c, Kind: "tart", Endpoint: options.Home, Worker: "local", DockerEndpoint: options.DockerEndpoint}
	store, e := engine.OpenStore(c.StateDir, settings.Scope())
	if e != nil {
		return nil, e
	}
	b, e := local.New(options, c)
	if e != nil {
		store.Close()
		return nil, e
	}
	return newRuntime(engine.Attach(settings, b, store), c), nil
}
func (r *Runtime) Submit(ctx context.Context, launches []api.Launch) ([]api.Submission, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrClosed
	}
	return r.p.Submit(ctx, launches)
}

// GetCapabilities returns the complete configured catalog in Config.Bases order.
// It does not inspect slot availability or contact the backend. Returned slices
// belong to the caller; no SSH credentials or image configuration are exposed.
func (r *Runtime) GetCapabilities(ctx context.Context) (api.Capabilities, error) {
	if err := ctx.Err(); err != nil {
		return api.Capabilities{}, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return api.Capabilities{}, err
	}
	if r.closed {
		return api.Capabilities{}, ErrClosed
	}
	out := api.Capabilities{BasesComplete: true, Bases: make([]api.BaseCapabilities, 0, len(r.p.Config.Bases))}
	for _, b := range r.p.Config.Bases {
		out.Bases = append(out.Bases, api.BaseCapabilities{Name: b.Name, Capabilities: append([]string{}, r.p.Config.BaseCapabilities(b.Name)...)})
	}
	return out, nil
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

func (r *Runtime) Images() ([]ImageStatus, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrClosed
	}
	return r.p.Images()
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

// Drain disables new admissions and retires slot reservations as jobs finish.
// Published images remain available for reuse or explicit PurgeImages.
// Call Reconcile until Slots and DockerAllocations are empty. It does not cancel active work.
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

// PurgeImages removes this journal's inactive prepared sources after Drain.
// Normal reconciliation, Close, and Drain leave published images available.
func (r *Runtime) PurgeImages(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	return r.p.PurgeImages(ctx)
}

// DockerAllocations reports warming, running and reclaiming Docker resources.
func (r *Runtime) DockerAllocations() ([]DockerStatus, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrClosed
	}
	return r.p.DockerAllocations()
}

type DockerStatus = engine.DockerStatus

// DefaultDockerEngine is the digest-pinned Ubuntu ARM64 profile qualified by the
// prototype. Enabling it and assigning a slot group remain explicit decisions.
func DefaultDockerEngine() DockerEngineProfile {
	return DockerEngineProfile{Name: "standard", Image: "ghcr.io/cirruslabs/ubuntu@sha256:439ba447488fa19f9b1ed091602546b8cb32442abdb60bac4943a4d6c3dd2ffa", SSHUsername: "admin", SSHPassword: "admin", CPU: 4, MemoryMiB: 8192, DiskGiB: 32, EngineVersion: "29.1.3"}
}
func DefaultLinuxBase() Base {
	p := DefaultDockerEngine()
	return Base{Name: "ubuntu-24.04-arm64", OS: "linux", OSVersion: "24.04", Image: p.Image, SSHUsername: p.SSHUsername, SSHPassword: p.SSHPassword, Capabilities: []string{"nix", "git", "node", "pnpm", "uv", "python"}}
}
