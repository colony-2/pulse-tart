package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	compute "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-tart/internal/backend"
	"github.com/colony-2/pulse-tart/internal/config"
)

type fake struct {
	mu               sync.Mutex
	vms              map[string]backend.VM
	creates, deletes int
	lose, readFail   bool
}

func (f *fake) Info(context.Context) error { return nil }
func (f *fake) List(context.Context) ([]backend.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readFail {
		return nil, errors.New("secret backend body")
	}
	v := []backend.VM{}
	for _, x := range f.vms {
		v = append(v, x)
	}
	return v, nil
}
func (f *fake) Create(_ context.Context, s backend.Spec) (backend.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	v := backend.VM{Spec: s, UID: newID(), Status: "pending"}
	f.vms[s.Name] = v
	if f.lose {
		return backend.VM{}, errors.New("lost response with secret")
	}
	return v, nil
}
func (f *fake) Get(_ context.Context, n string) (backend.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.vms[n]
	if !ok {
		return v, &backend.Error{Code: 404}
	}
	return v, nil
}
func (f *fake) Delete(_ context.Context, n string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	delete(f.vms, n)
	return nil
}
func fixture(t *testing.T) (*Provider, *fake) {
	t.Helper()
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	c, e := config.Normalize(config.Config{OwnerID: "test", StateDir: dir, GuestBundle: []byte("test-bundle"), Bases: []config.Base{{Name: "macos-sequoia-15.7.7-arm64", Aliases: []string{"macos", "darwin"}, Image: "example.test/macos@sha256:" + strings.Repeat("a", 64), OSVersion: "15.7.7", SSHPassword: "admin", Capabilities: []string{"nix", "git"}}}, Slots: []config.SlotGroup{{Name: "small", Count: 2, Base: "macos-sequoia-15.7.7-arm64"}}})
	if e != nil {
		t.Fatal(e)
	}
	f := &fake{vms: map[string]backend.VM{}}
	p, e := New(config.Settings{Config: c, Kind: "orchard", Endpoint: "http://example.test/v1", Worker: "mac-1"}, f)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { p.Close() })
	return p, f
}
func launch(id string) compute.Launch {
	return compute.Launch{LaunchID: id, Allocation: compute.Allocation{Base: "macos"}, Job: compute.JobRef{JobDB: "https://jobs.test/tenant", InstanceID: "db", TenantID: "tenant", JobID: "job"}, Lease: "LEASE-SENTINEL"}
}

func TestCellValidationAndNoSideEffects(t *testing.T) {
	p, f := slotsFixture(t, 2)
	warmSlots(t, p)
	cases := []struct {
		name   string
		change func(*compute.Launch)
		want   compute.Status
	}{
		{"empty base", func(l *compute.Launch) { l.Base = "" }, compute.Rejected},
		{"middle wildcard", func(l *compute.Launch) { l.Base = "mac*os" }, compute.Rejected},
		{"two wildcards", func(l *compute.Launch) { l.Base = "mac**" }, compute.Rejected},
		{"negative cpu", func(l *compute.Launch) { l.CPUMillis = -1 }, compute.Rejected},
		{"oversized memory", func(l *compute.Launch) { l.MemoryBytes = compute.MaxQuantity + 1 }, compute.Rejected},
		{"empty lease", func(l *compute.Launch) { l.Lease = "" }, compute.Rejected},
		{"credential URL", func(l *compute.Launch) { l.Job.JobDB = "https://user:password@jobs.test/tenant" }, compute.Rejected},
		{"empty identity", func(l *compute.Launch) { l.Job.InstanceID = "" }, compute.Rejected},
		{"runtime cap", func(l *compute.Launch) { l.TimeoutSeconds = 30 }, compute.Unsupported},
		{"scratch", func(l *compute.Launch) { l.ScratchBytes = 1 }, compute.Unsupported},
		{"capability", func(l *compute.Launch) { l.Capabilities = []string{"unknown"} }, compute.Unsupported},
		{"case", func(l *compute.Launch) { l.Base = "MacOS" }, compute.Unsupported},
		{"no implicit alias", func(l *compute.Launch) { l.Base = "macos-sequoia" }, compute.Unsupported},
	}
	for i, c := range cases {
		l := launch(fmt.Sprintf("validation-%d", i))
		c.change(&l)
		out, e := p.Submit(t.Context(), []compute.Launch{l})
		if e != nil || out[0].Status != c.want {
			t.Errorf("%s: %v %v", c.name, out, e)
		}
	}
	if f.creates != 2 {
		t.Fatal("decline created native work")
	}
	if _, e := p.Submit(t.Context(), []compute.Launch{launch("dup"), launch("dup")}); e == nil || f.creates != 2 {
		t.Fatal("invalid batch dispatched")
	}
}
func TestResolvedC2JInvocation(t *testing.T) {
	p, f := slotsFixture(t, 1)
	warmSlots(t, p)
	l := launch("invocation")
	l.Base = "darw*"
	l.CPUMillis = 1
	l.Capabilities = []string{"git", "nix", "git"}
	out, e := p.Submit(t.Context(), []compute.Launch{l})
	if e != nil || out[0].Status != compute.Accepted {
		t.Fatal(out, e)
	}
	r := p.Store.data.Records[l.LaunchID]
	v := f.vms[r.Name]
	script := v.StartupScript.Content
	parts := strings.Split(script, "PULSE_ENVELOPE'\n")
	if len(parts) != 2 {
		t.Fatal("missing envelope")
	}
	encoded := strings.SplitN(parts[1], "\n", 2)[0]
	b, e := base64.StdEncoding.DecodeString(encoded)
	if e != nil {
		t.Fatal(e)
	}
	var env envelope
	if json.Unmarshal(b, &env) != nil {
		t.Fatal("invalid envelope")
	}
	if string(env.Process.StdinBytes) != string(l.Lease) || len(env.Process.Command) != 1 || env.Process.Command[0] != "/opt/pulse-tart/bin/c2j" {
		t.Fatal("wrong worker or altered lease")
	}
	args := env.Process.Args
	joined := strings.Join(args, " ")
	if strings.Contains(joined, string(l.Lease)) || strings.Contains(joined, "darw*") {
		t.Fatal("lease or selector passed as allocation fact")
	}
	for _, want := range []string{"run with-lease", "--lease-file -", "--execution-cpu 4000m", "--execution-memory 8192Mi", "--execution-platform darwin/arm64", "--execution-image " + p.Config.Bases[0].Image, "--execution-capability nix"} {
		if !strings.Contains(joined, want) {
			t.Error("missing", want)
		}
	}
	for _, value := range env.Process.Env {
		if strings.Contains(value, string(l.Lease)) {
			t.Fatal("lease in environment")
		}
	}
	if v.StartupScript.Env["PULSE_JOB_ID"] != l.Job.JobID || v.StartupScript.Env["PULSE_JOBDB"] != l.Job.JobDB {
		t.Fatal("missing native correlation")
	}
	for key := range v.Labels {
		if strings.HasPrefix(key, "dev.pulse.") {
			t.Fatal("correlation incorrectly became worker selector")
		}
	}
	// Capability order and duplicates do not change assignment identity.
	l.Capabilities = []string{"nix", "git"}
	out, _ = p.Submit(t.Context(), []compute.Launch{l})
	if out[0].Status != compute.Accepted || f.creates != 2 {
		t.Fatal("semantic duplicate replayed")
	}
	l.Lease = "changed"
	out, _ = p.Submit(t.Context(), []compute.Launch{l})
	if out[0].Status != compute.Rejected {
		t.Fatal("conflicting lease accepted")
	}
	page, e := p.List(t.Context(), compute.ListRequest{})
	if e != nil || len(page.Items) != 1 || page.Items[0].Job != l.Job {
		t.Fatal("JobRef not preserved")
	}
	data, _ := json.Marshal(page)
	if strings.Contains(string(data), "LEASE-SENTINEL") {
		t.Fatal("listing leaked lease")
	}
}
func TestReadOnlyListAndSafeCleanup(t *testing.T) {
	p, f := slotsFixture(t, 1)
	warmSlots(t, p)
	_, _ = p.Submit(t.Context(), []compute.Launch{launch("running")})
	r := p.Store.data.Records["running"]
	v := f.vms[r.Name]
	v.Status = "failed"
	v.Conditions = []backend.Condition{{Type: "running", State: "true"}}
	f.vms[r.Name] = v
	before, _ := os.ReadFile(filepath.Join(p.Config.StateDir, "state.json"))
	page, e := p.List(t.Context(), compute.ListRequest{})
	after, _ := os.ReadFile(filepath.Join(p.Config.StateDir, "state.json"))
	if e != nil || len(page.Items) != 1 || page.Items[0].State != "running" || !bytes.Equal(before, after) || f.deletes != 0 {
		t.Fatal("listing mutated or hid active work")
	}
	if e = p.Reconcile(t.Context()); e != nil || f.deletes != 0 {
		t.Fatal("failed status killed zero-timeout worker")
	}
	f.readFail = true
	if page, e = p.List(t.Context(), compute.ListRequest{}); e == nil || len(page.Items) != 0 {
		t.Fatal("observation failure became successful inventory")
	}
	f.readFail = false
	v.Conditions[0].State = "false"
	f.vms[r.Name] = v
	if e = p.Reconcile(t.Context()); e != nil || f.deletes != 1 {
		t.Fatal("stopped job not removed")
	}
}
func TestConcurrentDuplicateAndPagination(t *testing.T) {
	p, f := slotsFixture(t, 2)
	warmSlots(t, p)
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, e := p.Submit(t.Context(), []compute.Launch{launch("same")})
			if e != nil || out[0].Status != compute.Accepted {
				t.Error(out, e)
			}
		}()
	}
	wg.Wait()
	if f.creates != 3 {
		t.Fatal("duplicate native launches")
	}
	_, _ = p.Submit(t.Context(), []compute.Launch{launch("other")})
	first, e := p.List(t.Context(), compute.ListRequest{PageSize: 1})
	if e != nil || len(first.Items) != 1 || first.NextPageToken == "" {
		t.Fatal("no first page")
	}
	second, e := p.List(t.Context(), compute.ListRequest{PageSize: 1, PageToken: first.NextPageToken})
	if e != nil || len(second.Items) != 1 || first.Items[0].ID == second.Items[0].ID || second.NextPageToken != "" {
		t.Fatal("invalid second page")
	}
	if _, e = p.List(t.Context(), compute.ListRequest{PageToken: first.NextPageToken, LaunchID: "same"}); e == nil {
		t.Fatal("cursor accepted for different filter")
	}
	if _, e = p.List(t.Context(), compute.ListRequest{PageSize: 101}); e == nil {
		t.Fatal("bad page size accepted")
	}
}
func TestStoreExclusionAndCorruption(t *testing.T) {
	p, f := fixture(t)
	if other, e := New(p.Config, f); e == nil {
		other.Close()
		t.Fatal("duplicate state owner")
	}
	p.Close()
	path := filepath.Join(p.Config.StateDir, "state.json")
	if e := os.WriteFile(path, []byte("{"), 0600); e != nil {
		t.Fatal(e)
	}
	if other, e := New(p.Config, f); e == nil {
		other.Close()
		t.Fatal("corrupt state accepted")
	}
}

func TestCanceledCallsDoNotWaitForOtherDispatch(t *testing.T) {
	p, _ := fixture(t)
	p.Store.mu.Lock()
	defer p.Store.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, e := p.List(ctx, compute.ListRequest{}); e == nil {
			t.Error("List ignored cancellation")
		}
		if _, e := p.Submit(ctx, []compute.Launch{launch("cancel")}); e == nil {
			t.Error("Submit ignored cancellation")
		}
		if e := p.Reconcile(ctx); e == nil {
			t.Error("Reconcile ignored cancellation")
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled call waited behind native dispatch")
	}
}

func TestOpaqueLeaseFingerprint(t *testing.T) {
	p, _ := fixture(t)
	a := launch("binary")
	a.Lease = compute.Lease("opaque\x00\xff")
	b := a
	b.Lease = compute.Lease("opaque\x00\xfe")
	if !validLaunch(a) || !validLaunch(b) {
		t.Fatal("opaque lease interpreted")
	}
	if p.launchHash(a) == p.launchHash(b) {
		t.Fatal("different credentials collapsed")
	}
	process := workerProcess(a, p.Config.Bases[0], 4, 8192)
	data, _ := json.Marshal(process)
	var restored processForTest
	_ = json.Unmarshal(data, &restored)
	if string(restored.StdinBytes) != string(a.Lease) {
		t.Fatal("lease changed in envelope")
	}
}

type processForTest struct {
	StdinBytes []byte `json:"stdin_bytes"`
}
