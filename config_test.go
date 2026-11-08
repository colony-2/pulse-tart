package tart

import (
	"os"
	"reflect"
	"testing"
)

func TestCountConventions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		counts VMCounts
		want   [3]int
	}{
		{"omitted", VMCounts{}, [3]int{2, 4, 6}},
		{"macOS only", VMCounts{MacOS: VMCount(1)}, [3]int{1, 4, 5}},
		{"Linux only", VMCounts{Linux: VMCount(6)}, [3]int{2, 6, 8}},
		{"shared total", VMCounts{Total: VMCount(3)}, [3]int{2, 4, 3}},
		{"disable Linux", VMCounts{Linux: VMCount(0)}, [3]int{2, 0, 2}},
		{"disable macOS", VMCounts{MacOS: VMCount(0)}, [3]int{0, 4, 4}},
		{"disable both", VMCounts{MacOS: VMCount(0), Linux: VMCount(0)}, [3]int{}},
		{"drain total", VMCounts{Total: VMCount(0)}, [3]int{2, 4, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ResolveConfig(Config{OwnerID: "defaults", StateDir: t.TempDir(), VMCounts: tc.counts})
			if err != nil {
				t.Fatal(err)
			}
			if got := [3]int{*c.VMCounts.MacOS, *c.VMCounts.Linux, *c.VMCounts.Total}; got != tc.want {
				t.Fatalf("counts = %v, want %v", got, tc.want)
			}
			if len(c.Slots) != 2 || c.Slots[0].Base != "macos-26-arm64" || c.Slots[1].Base != "ubuntu-24.04-arm64" {
				t.Fatal("unexpected conventional pools", c.Slots)
			}
			if c.Slots[0].Count != min(tc.want[0], tc.want[2]) || c.Slots[1].Count != min(tc.want[1], tc.want[2]) {
				t.Fatal("pool count disagrees with limits", c.Slots)
			}
			for _, g := range c.Slots {
				if g.CPU != 4 || g.MemoryMiB != 8192 {
					t.Fatal("unexpected VM size", g)
				}
			}
			if c.Slots[0].DockerProfile != "" || c.Slots[1].DockerProfile == "" || c.NeedsDockerEndpoint() {
				t.Fatal("Linux defaults require macOS Docker connectivity")
			}
			again, err := ResolveConfig(c)
			if err != nil || !reflect.DeepEqual(c, again) {
				t.Fatal("resolving defaults is not idempotent", err)
			}
		})
	}
}

func TestCountsAndSizingRejectedBeforeOpeningState(t *testing.T) {
	for _, change := range []func(*Config){
		func(c *Config) { c.VMCounts.MacOS = VMCount(3) },
		func(c *Config) { c.VMCounts.Linux = VMCount(-1) },
		func(c *Config) { c.VMCounts.Total = VMCount(-1) },
		func(c *Config) { c.Slots[0].CPU = 8 },
		func(c *Config) { c.Slots[0].MemoryMiB = 16384 },
		func(c *Config) { c.Docker.Profiles[0].CPU = 2 },
		func(c *Config) { c.Docker.Profiles[0].MemoryMiB = 4096 },
	} {
		c, err := ResolveConfig(Config{OwnerID: "invalid", StateDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		change(&c)
		if _, err := ResolveConfig(c); err == nil {
			t.Fatal("unsupported count or VM size accepted")
		}
	}
}

func TestDefaultOrchardAndCountCopies(t *testing.T) {
	counts := VMCounts{MacOS: VMCount(1), Linux: VMCount(2), Total: VMCount(2)}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	r, err := NewOrchard(Config{OwnerID: "defaults", StateDir: dir, VMCounts: counts}, OrchardConfig{URL: "https://orchard.example.test", Worker: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	*counts.MacOS, *counts.Linux, *counts.Total = 0, 0, 0
	if *r.p.Config.VMCounts.MacOS != 1 || *r.p.Config.VMCounts.Linux != 2 || *r.p.Config.VMCounts.Total != 2 {
		t.Fatal("retained caller-owned counts")
	}
	caps, err := r.GetCapabilities(t.Context())
	if err != nil || len(caps.Bases) != 2 {
		t.Fatal("missing conventional base catalog", caps, err)
	}
}

func TestConventionalMacDockerRemainsExplicit(t *testing.T) {
	c, err := ResolveConfig(Config{OwnerID: "docker", StateDir: t.TempDir(), Docker: &DockerConfig{}})
	if err != nil {
		t.Fatal(err)
	}
	if !c.NeedsDockerEndpoint() || c.Slots[0].DockerProfile != "standard" || c.Slots[1].DockerProfile != "standard" {
		t.Fatal("explicit Docker did not enable both pools", c.Slots)
	}
	c, err = ResolveConfig(Config{OwnerID: "linux", StateDir: t.TempDir(), Docker: &DockerConfig{}, VMCounts: VMCounts{MacOS: VMCount(0)}})
	if err != nil || c.NeedsDockerEndpoint() {
		t.Fatal("disabled macOS requires an endpoint", err)
	}
}
