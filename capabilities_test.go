package tart

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	api "github.com/colony-2/pulse-provider-api"
)

func TestGetCapabilities(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "backend offline", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	c := testConfig(t)
	c.Bases[0].Name = "macos-z-preferred"
	c.Bases[0].Capabilities = []string{"nix", "git", "git"}
	second := c.Bases[0]
	second.Name, second.Capabilities = "macos-a-secondary", nil
	c.Bases = append(c.Bases, second)
	c.Slots[0].Base = c.Bases[0].Name
	c.Slots = append(c.Slots, SlotGroup{Name: "disabled", Base: second.Name, Count: 0})
	r, err := NewOrchard(c, OrchardConfig{URL: server.URL, Worker: "mac", AllowInsecureHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	before, err := os.ReadFile(filepath.Join(c.StateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := api.Capabilities{BasesComplete: true, Bases: []api.BaseCapabilities{
		{Name: "macos-z-preferred", Capabilities: []string{"git", "nix"}},
		{Name: "macos-a-secondary", Capabilities: []string{}},
	}}
	// Configured support is available before preparation and with disabled slots.
	var provider api.Provider = r
	got, err := provider.GetCapabilities(t.Context())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog: %#v, %v", got, err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{string(c.Bases[0].SSHPassword), c.Bases[0].Image, server.URL, c.StateDir} {
		if strings.Contains(string(encoded), private) {
			t.Fatal("private configuration in catalog")
		}
	}
	got.Bases[0].Name = "changed"
	got.Bases[0].Capabilities[0] = "changed"
	c.Bases[0].Name = "changed"
	c.Bases[0].Capabilities[0] = "changed"
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			result, err := provider.GetCapabilities(t.Context())
			if err != nil || !reflect.DeepEqual(result, want) {
				t.Errorf("shared or changed catalog: %#v, %v", result, err)
				return
			}
			result.Bases[0].Capabilities[0] = "caller-owned"
		})
	}
	wg.Wait()
	after, err := os.ReadFile(filepath.Join(c.StateDir, "state.json"))
	if err != nil || string(after) != string(before) || calls.Load() != 0 {
		t.Fatal("discovery contacted backend or mutated journal", err)
	}
	slots, err := r.Slots()
	if err != nil || len(slots) != 0 {
		t.Fatal("discovery prepared slots", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err = provider.GetCapabilities(ctx)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, api.Capabilities{}) {
		t.Fatal("canceled discovery returned support", got, err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	got, err = provider.GetCapabilities(t.Context())
	if !errors.Is(err, ErrClosed) || !reflect.DeepEqual(got, api.Capabilities{}) {
		t.Fatal("closed discovery returned support", got, err)
	}
}
