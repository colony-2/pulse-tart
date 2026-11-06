package docker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	api "github.com/colony-2/pulse-provider-api"
)

// Config describes ONE disposable, prepared Linux VM or macOS/Linux pair.
// Its VM must never be assigned another job. The experiment harness owns native
// provisioning/deletion. A production pool/pair allocator is not implemented.
type Config struct {
	Enabled                         bool
	StateDir, Base, Image, NativeID string
	CPU, MemoryMiB                  int64
	Guest                           Guest
	Workspace                       string
	Engine                          Guest
	EngineNativeID                  string
	platform                        string
}
type Provider struct {
	mu     sync.Mutex
	cfg    Config
	file   *os.File
	entry  *entry
	closed bool
}
type entry struct {
	ID, Hash string
	Job      api.JobRef
	Status   api.Status
}

var _ api.Provider = (*Provider)(nil)

func NewLinux(ctx context.Context, c Config) (*Provider, error) {
	c.platform = "linux"
	if c.Workspace == "" {
		c.Workspace = "/Users/pulseworker/work"
	}
	return newAllocation(ctx, c)
}

// NewMacOS verifies a prepared macOS primary, its private Linux companion and
// the relayed engine identity before advertising Docker. Provisioning is owned by
// the experimental harness. No lease is delivered to the companion.
func NewMacOS(ctx context.Context, c Config) (*Provider, error) {
	c.platform = "darwin"
	if c.Workspace == "" {
		c.Workspace = "/Users/pulseworker/work"
	}
	if c.EngineNativeID == "" || c.Engine.VM == "" || c.Engine.VM == c.Guest.VM || (c.Workspace != "" && !regexp.MustCompile(`^/Users/pulseworker/work(?:/[A-Za-z0-9_-]+)*$`).MatchString(c.Workspace)) {
		return nil, fmt.Errorf("invalid macOS paired allocation")
	}
	return newAllocation(ctx, c)
}
func newAllocation(ctx context.Context, c Config) (*Provider, error) {
	basePattern := `^ubuntu-[0-9.]+-arm64$`
	if c.platform == "darwin" {
		basePattern = `^macos-tahoe-[0-9.]+-arm64$`
	}

	if !c.Enabled || !filepath.IsAbs(c.StateDir) || c.CPU < 1 || c.CPU > 1024 || c.MemoryMiB < 1024 || c.MemoryMiB > 1048576 || !regexp.MustCompile(basePattern).MatchString(c.Base) || !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.:/_-]*@sha256:[a-f0-9]{64}$`).MatchString(c.Image) || c.NativeID == "" {
		return nil, fmt.Errorf("invalid or disabled experimental allocation configuration")
	}
	if err := os.MkdirAll(c.StateDir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(c.StateDir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("experimental state is already in use")
	}
	p := &Provider{cfg: c, file: f}
	scope, _ := json.Marshal([]any{c.NativeID, c.Guest.VM, c.Guest.OrchardURL, c.Guest.Address, c.Base, c.Image, c.CPU, c.MemoryMiB, c.platform, c.Workspace, c.EngineNativeID, c.Engine.VM, c.Engine.OrchardURL})
	sum := sha256.Sum256(scope)
	scope = []byte(hex.EncodeToString(sum[:]))
	scopeFile := filepath.Join(c.StateDir, "scope")
	if previous, err := os.ReadFile(scopeFile); err == nil {
		if !bytes.Equal(previous, scope) {
			p.Close()
			return nil, fmt.Errorf("allocation scope changed")
		}
	} else if os.IsNotExist(err) {
		if err = os.WriteFile(scopeFile, scope, 0600); err != nil {
			p.Close()
			return nil, err
		}
	} else {
		p.Close()
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(c.StateDir, "launch.json"))
	if err == nil {
		var e entry
		if json.Unmarshal(raw, &e) != nil || e.ID == "" || e.Hash == "" {
			p.Close()
			return nil, fmt.Errorf("invalid experimental journal")
		}
		p.entry = &e
	} else if !os.IsNotExist(err) {
		p.Close()
		return nil, err
	}
	// Verify allocation facts before advertising Docker or handing off a lease.
	probe := fmt.Sprintf("test \"$(uname -s)/$(uname -m)\" = Linux/aarch64 && test \"$(nproc)\" -ge %d && test \"$(awk '/MemTotal/{print $2}' /proc/meminfo)\" -ge %d && sudo -u pulseworker docker --host unix:///var/run/docker.sock info >/dev/null && sudo -u pulseworker docker buildx version >/dev/null && sudo -u pulseworker docker compose version >/dev/null && test -x /opt/pulse-docker/c2j", c.CPU, c.MemoryMiB*1024)
	probe = ". /etc/os-release; test \"$ID\" = ubuntu && test \"$VERSION_ID\" = " + strings.TrimSuffix(strings.TrimPrefix(c.Base, "ubuntu-"), "-arm64") + " && " + probe + " && git --version >/dev/null && cat /proc/sys/kernel/random/boot_id && sudo -u pulseworker docker --host unix:///var/run/docker.sock info --format '{{.ID}}'"
	if c.platform == "darwin" {
		probe = macOSProbe(c)
	}
	var identity, diagnostics bytes.Buffer
	if err = c.Guest.Exec(ctx, probe, nil, &identity, &diagnostics); err != nil {
		p.Close()
		return nil, verificationFailure("Docker allocation verification failed", "PULSE_PREFLIGHT_FAILED=", diagnostics.String())
	}
	if c.platform == "darwin" {
		var engineIdentity bytes.Buffer
		err = c.Engine.Exec(ctx, `test "$(uname -s)/$(uname -m)" = Linux/aarch64 && sudo -u pulseworker docker info --format '{{.ID}}'`, nil, &engineIdentity, io.Discard)
		primaryIDs := strings.Fields(identity.String())
		if err != nil || len(primaryIDs) != 2 || strings.TrimSpace(engineIdentity.String()) != primaryIDs[1] {
			p.Close()
			return nil, fmt.Errorf("primary relay does not identify the configured private engine")
		}
	}
	identityFile := filepath.Join(c.StateDir, "engine-identity")
	if previous, err := os.ReadFile(identityFile); err == nil {
		if !bytes.Equal(previous, identity.Bytes()) {
			p.Close()
			return nil, fmt.Errorf("allocation engine or boot identity changed")
		}
	} else if os.IsNotExist(err) {
		if err = os.WriteFile(identityFile, identity.Bytes(), 0600); err != nil {
			p.Close()
			return nil, err
		}
	} else {
		p.Close()
		return nil, err
	}
	return p, nil
}
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	return p.file.Close()
}
func (p *Provider) GetCapabilities(ctx context.Context) (api.Capabilities, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ctx.Err() != nil {
		return api.Capabilities{}, ctx.Err()
	}
	if p.closed {
		return api.Capabilities{}, fmt.Errorf("closed")
	}
	return api.Capabilities{BasesComplete: true, Bases: []api.BaseCapabilities{{Name: p.cfg.Base, Capabilities: []string{"docker", "git"}}}}, nil
}
func (p *Provider) save() error {
	b, _ := json.Marshal(p.entry)
	f, err := os.CreateTemp(p.cfg.StateDir, ".intent-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(p.cfg.StateDir, "launch.json")); err != nil {
		return err
	}
	d, err := os.Open(p.cfg.StateDir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (p *Provider) executionPlatform() string {
	if p.cfg.platform == "darwin" {
		return "darwin/arm64"
	}
	return "linux/arm64"
}
func validEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && u.Fragment == ""
}
func match(pattern, value string) bool {
	r := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*") + "$"
	ok, _ := regexp.MatchString(r, value)
	return ok
}
func (p *Provider) Submit(ctx context.Context, ls []api.Launch) ([]api.Submission, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, fmt.Errorf("closed")
	}
	if len(ls) == 0 || len(ls) > api.MaxBatch {
		return nil, fmt.Errorf("invalid batch")
	}
	seen := map[string]bool{}
	for _, l := range ls {
		if seen[l.LaunchID] {
			return nil, fmt.Errorf("duplicate batch ID")
		}
		seen[l.LaunchID] = true
	}
	out := []api.Submission{}
	for _, l := range ls {
		result := api.Submission{LaunchID: l.LaunchID, Status: api.Rejected, Reason: "invalid launch"}
		if !regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`).MatchString(l.LaunchID) || l.Lease == "" || l.Job.JobID == "" || l.Job.InstanceID == "" || l.Job.TenantID == "" || !validEndpoint(l.Job.JobDB) || l.Base == "" || l.CPUMillis < 0 || l.CPUMillis > api.MaxQuantity || l.MemoryBytes < 0 || l.MemoryBytes > api.MaxQuantity || l.ScratchBytes < 0 || l.ScratchBytes > api.MaxQuantity || l.TimeoutSeconds < 0 || l.TimeoutSeconds > 31536000 {
			out = append(out, result)
			continue
		}
		if !match(l.Base, p.cfg.Base) || l.CPUMillis > p.cfg.CPU*1000 || l.MemoryBytes > p.cfg.MemoryMiB<<20 || l.ScratchBytes != 0 || l.TimeoutSeconds != 0 || l.StartBefore != nil || slices.ContainsFunc(l.Capabilities, func(s string) bool { return s != "docker" && s != "git" }) {
			result.Status = api.Unsupported
			result.Reason = "unsupported experimental allocation"
			out = append(out, result)
			continue
		}
		lease := []byte(l.Lease)
		l.Lease = ""
		b, _ := json.Marshal(struct {
			Launch api.Launch
			Lease  []byte
		}{l, lease})
		sum := sha256.Sum256(b)
		hash := hex.EncodeToString(sum[:])
		if p.entry != nil {
			if p.entry.ID == l.LaunchID && p.entry.Hash == hash {
				result.Status = p.entry.Status
				result.Reason = ""
				if result.Status == api.Unknown {
					result.Reason = "handoff uncertain"
				}
			} else if p.entry.ID != l.LaunchID {
				result.Status = api.NoCapacity
				result.Reason = "single-use allocation already reserved"
			}
			out = append(out, result)
			continue
		}
		if ctx.Err() != nil {
			result.Status = api.Unavailable
			result.Reason = "canceled before reservation"
			out = append(out, result)
			continue
		}
		p.entry = &entry{ID: l.LaunchID, Hash: hash, Job: l.Job, Status: api.Unknown}
		if err := p.save(); err != nil {
			return out, err
		}
		args := []string{"/opt/pulse-docker/c2j", "run", "with-lease", "--jobdb", l.Job.JobDB, "--job-id", l.Job.JobID, "--lease-file", "-", "--ci", "--input-mode", "fail", "--execution-base", p.cfg.Base, "--execution-cpu", strconv.FormatInt(p.cfg.CPU*1000, 10) + "m", "--execution-memory", strconv.FormatInt(p.cfg.MemoryMiB, 10) + "Mi", "--execution-platform", p.executionPlatform(), "--execution-image", p.cfg.Image, "--execution-image-digest", strings.Split(p.cfg.Image, "@")[1], "--execution-capability", "docker", "--execution-capability", "git"}
		payload, _ := json.Marshal(map[string]any{"id": l.LaunchID, "hash": hash, "args": args, "lease": base64.StdEncoding.EncodeToString(lease), "workspace": p.cfg.Workspace})
		var response bytes.Buffer
		err := p.cfg.Guest.Exec(ctx, "sudo /usr/bin/python3 /opt/pulse-docker/accept.py", bytes.NewReader(payload), &response, io.Discard)
		result.Status = api.Unknown
		result.Reason = "handoff uncertain"
		if err == nil && strings.TrimSpace(response.String()) == "PULSE_DOCKER_ACCEPTED" {
			p.entry.Status = api.Accepted
			result.Status = api.Accepted
			result.Reason = ""
		}
		if err = p.save(); err != nil {
			return append(out, result), err
		}
		out = append(out, result)
	}
	return out, nil
}
func (p *Provider) List(ctx context.Context, q api.ListRequest) (api.ListResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := api.ListResponse{Items: []api.Instance{}}
	if p.closed {
		return out, fmt.Errorf("closed")
	}
	if q.PageToken != "" || q.PageSize < 0 || q.PageSize > 100 {
		return out, fmt.Errorf("invalid page")
	}
	if p.entry == nil || (q.LaunchID != "" && q.LaunchID != p.entry.ID) {
		return out, nil
	}
	if p.cfg.platform == "darwin" {
		if err := p.cfg.Engine.Exec(ctx, "true", nil, io.Discard, io.Discard); err != nil {
			return out, fmt.Errorf("companion observation unavailable")
		}
	}
	var b bytes.Buffer
	if err := p.cfg.Guest.Exec(ctx, "if sudo test -f /var/lib/pulse-docker/exit; then echo done; else echo active; fi", nil, &b, io.Discard); err != nil {
		return out, fmt.Errorf("allocation observation unavailable")
	}
	if strings.TrimSpace(b.String()) != "done" {
		out.Items = append(out.Items, api.Instance{ID: p.cfg.Guest.VM, LaunchID: p.entry.ID, Job: p.entry.Job, State: "running", Refs: []string{p.cfg.Guest.VM}})
		if p.cfg.platform == "darwin" {
			out.Items = append(out.Items, api.Instance{ID: p.cfg.Engine.VM, LaunchID: p.entry.ID, Job: p.entry.Job, State: "running", Refs: []string{p.cfg.Engine.VM}})
		}
	}
	return out, nil
}
