package tart

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-tart/internal/backend"
	"github.com/colony-2/pulse-tart/internal/bundle"
	"github.com/colony-2/pulse-tart/internal/orchard"
)

const liveImage = "ghcr.io/cirruslabs/macos-sequoia-base@sha256:eee659985b01e4df2a6c0754bd8d25e54969933c43fdcd4edef8bea337b3d354"

// TestLive exercises slot lifecycle and real JobDB jobs through the public API.
func TestLive(t *testing.T) {
	kind := os.Getenv("PULSE_TEST_BACKEND")
	if kind == "" {
		t.Skip("set PULSE_TEST_BACKEND=orchard or tart; see README")
	}
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	owner := "test-" + hex.EncodeToString(nonce[:])
	dir, e := os.MkdirTemp("", "pulse-cell-"+owner+"-")
	if e != nil {
		t.Fatal(e)
	}
	image := os.Getenv("PULSE_TEST_IMAGE")
	if image == "" {
		image = liveImage
	}
	version := os.Getenv("PULSE_TEST_OS_VERSION")
	if version == "" {
		version = "15.7.7"
	}
	password := os.Getenv("PULSE_TEST_SSH_PASSWORD")
	if password == "" {
		password = "admin"
	}
	c := Config{OwnerID: owner, StateDir: dir, Bases: []Base{{Name: "macos-test-" + version + "-arm64", Aliases: []string{"macos"}, Image: image, OSVersion: version, SSHPassword: Secret(password), Capabilities: []string{"nix", "git", "node", "pnpm", "uv", "python"}}}, Slots: []SlotGroup{{Name: "standard", Count: 2, Base: "macos-test-" + version + "-arm64"}}}
	archive, e := bundle.Build(assets, "integration")
	if e != nil {
		t.Fatal(e)
	}
	c.GuestBundle = archive
	report := newLiveReport(t, password)
	connect := OrchardConfig{URL: os.Getenv("PULSE_TEST_ORCHARD_URL"), Worker: os.Getenv("PULSE_TEST_ORCHARD_WORKER"), AllowInsecureHTTP: true}
	factory := func() (*Runtime, error) {
		switch kind {
		case "orchard":
			return NewOrchard(c, connect)
		case "tart":
			return NewTart(c, TartConfig{Home: os.Getenv("PULSE_TEST_TART_HOME")})
		default:
			return nil, fmt.Errorf("unknown test backend")
		}
	}
	r, e := factory()
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = r.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Minute)
	defer cancel()
	t.Logf("state directory: %s", dir)
	// Cleanup only this journal's exact identities. Drain never kills a valid job.
	defer func() {
		clean, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		report.capture(t, clean, kind, connect, r, dir)
		_ = r.Drain(clean)
		for clean.Err() == nil {
			if e := r.Reconcile(clean); e != nil {
				time.Sleep(time.Second)
				continue
			}
			slots, _ := r.Slots()
			page, e := r.List(clean, api.ListRequest{})
			if e == nil && len(slots) == 0 && len(page.Items) == 0 {
				_ = r.Close()
				if !t.Failed() {
					_ = os.RemoveAll(dir)
				} else {
					t.Logf("private failure state retained at %s", dir)
				}
				return
			}
			time.Sleep(time.Second)
		}
		t.Errorf("cleanup incomplete; preserve journal at %s for recovery", dir)
	}()
	start := time.Now()
	waitLive(t, ctx, "prepared suspended slots", 30*time.Minute, func(ctx context.Context) bool {
		if e := r.Reconcile(ctx); e != nil {
			t.Fatal(e)
		}
		slots, e := r.Slots()
		if e != nil {
			t.Fatal(e)
		}
		ready := 0
		for _, s := range slots {
			if s.Phase == "failed" || s.Phase == "quarantine" {
				t.Fatalf("template %s entered %s (see vm-diagnostics.json)", s.Template, s.Phase)
			}
			if s.Phase == "ready" {
				ready++
			}
		}
		return ready == 2
	})
	t.Logf("two templates ready in %s", time.Since(start).Round(time.Millisecond))
	slots, _ := r.Slots()
	if slots[0].MAC == slots[1].MAC || slots[0].Template == slots[1].Template {
		t.Fatal("templates share an identity")
	}
	launch := func(id string) api.Launch {
		return api.Launch{LaunchID: owner + "-" + id, Allocation: api.Allocation{Base: "macos*", CPUMillis: 1000, MemoryBytes: 2 << 30, Capabilities: []string{"nix", "git"}}, Job: api.JobRef{JobDB: "http://127.0.0.1:1/test", InstanceID: "fixture", TenantID: "test", JobID: "invalid-lease"}, Lease: "deliberately-invalid-integration-lease"}
	}
	a, b := launch("a"), launch("b")
	start = time.Now()
	out, e := r.Submit(ctx, []api.Launch{a, b, launch("overflow")})
	if e != nil || len(out) != 3 || out[0].Status != api.Accepted || out[1].Status != api.Accepted || out[2].Status != api.NoCapacity {
		t.Fatal("admission statuses", out, e)
	}
	// Restart the library while both native VMs are starting/running.
	if e = r.Close(); e != nil {
		t.Fatal(e)
	}
	r, e = factory()
	if e != nil {
		t.Fatal(e)
	}
	out, e = r.Submit(ctx, []api.Launch{a, b})
	if e != nil || out[0].Status != api.Accepted || out[1].Status != api.Accepted {
		t.Fatal("replay created conflicting admissions", out, e)
	}
	done := func(ls []api.Launch) {
		t.Helper()
		for _, l := range ls {
			l := l
			waitLive(t, ctx, "guest handoff and c2j exit: "+l.LaunchID, 10*time.Minute, func(ctx context.Context) bool {
				if recoverer, ok := r.p.Backend.(interface{ Recover(context.Context) error }); ok {
					_ = recoverer.Recover(ctx)
				}
				data, err := os.ReadFile(filepath.Join(dir, "state.json"))
				if err != nil {
					t.Fatal(err)
				}
				var journal struct {
					Records map[string]struct {
						Name string `json:"name"`
					} `json:"records"`
				}
				if json.Unmarshal(data, &journal) != nil {
					t.Fatal("invalid test journal")
				}
				record := journal.Records[l.LaunchID].Name
				events, e := r.p.Backend.(interface {
					Events(context.Context, string) ([]backend.Event, error)
				}).Events(ctx, record)
				if e != nil {
					t.Fatal(e)
				}
				text := ""
				for _, event := range events {
					text += event.Payload + "\n" + event.Message + "\n"
				}
				if strings.Contains(text, "PULSE_TEMPLATE_RESTORE_FAILED") {
					t.Fatal("clone cold-booted")
				}
				if !strings.Contains(text, "PULSE_HANDOFF_OK") {
					return false
				}
				expected := "PULSE_WORKER_FAILED"
				if strings.HasSuffix(l.LaunchID, "-jobdb-valid") {
					expected = "PULSE_WORKER_OK"
				}
				opposite := "PULSE_WORKER_OK"
				if expected == opposite {
					opposite = "PULSE_WORKER_FAILED"
				}
				if strings.Contains(text, opposite) {
					t.Fatal("c2j exit did not match expected lease outcome")
				}
				if !strings.Contains(text, expected) {
					return false
				}
				v, e := r.p.Backend.Get(ctx, record)
				if e != nil {
					t.Fatal(e)
				}
				return v.Stopped() && v.Status == "failed"
			})
		}
	}
	// Listing must preserve the caller's identity and exclude templates.
	page, e := r.List(ctx, api.ListRequest{})
	if e != nil {
		t.Fatal(e)
	}
	for _, item := range page.Items {
		if item.LaunchID == a.LaunchID && item.Job != a.Job {
			t.Fatal("JobRef changed")
		}
	}
	done([]api.Launch{a, b})
	t.Logf("two resumed clones handed off and exited in %s", time.Since(start).Round(time.Millisecond))
	if e = r.Reconcile(ctx); e != nil {
		t.Fatal(e)
	}
	again := launch("reuse")
	out, e = r.Submit(ctx, []api.Launch{again})
	if e != nil || out[0].Status != api.Accepted {
		t.Fatal("slot not reusable", out, e)
	}
	done([]api.Launch{again})
	if e = r.Reconcile(ctx); e != nil {
		t.Fatal(e)
	}
	exerciseJobDB(t, ctx, kind, connect, r, owner, dir, report, done)
	data, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if strings.Contains(string(data), string(a.Lease)) {
		t.Fatal("lease leaked to fingerprint journal")
	}
	if kind == "orchard" {
		client, _ := orchard.New(connect)
		if e = client.Info(ctx); e != nil {
			t.Fatal(e)
		}
	}
}

// Every live wait has a phase-specific deadline and visible progress. Passing
// the bounded context to probes also stops network calls when the phase expires.
func waitLive(t *testing.T, parent context.Context, label string, timeout time.Duration, ready func(context.Context) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	start := time.Now()
	nextLog := start.Add(30 * time.Second)
	t.Logf("waiting for %s (limit %s)", label, timeout)
	for ctx.Err() == nil {
		if ready(ctx) {
			t.Logf("ready: %s after %s", label, time.Since(start).Round(time.Second))
			return
		}
		if !time.Now().Before(nextLog) {
			t.Logf("still waiting for %s after %s", label, time.Since(start).Round(time.Second))
			nextLog = time.Now().Add(30 * time.Second)
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	t.Fatalf("timeout waiting for %s after %s", label, time.Since(start).Round(time.Second))
}
