package config

import (
	"strings"
	"testing"
)

func dockerConfig() Config {
	return Config{OwnerID: "owner", StateDir: "/tmp/pulse", Bases: []Base{{Name: "macos-26-arm64", Image: "x@sha256:" + strings.Repeat("a", 64), OSVersion: "26.6.2", SSHPassword: "admin", Capabilities: []string{"git"}}}, Slots: []SlotGroup{{Name: "docker", Base: "macos-26-arm64", Count: 1, DockerProfile: "standard"}}, Docker: &Docker{Profiles: []EngineProfile{{Name: "standard", Image: "x@sha256:" + strings.Repeat("b", 64), SSHPassword: "admin", SSHHostKey: []byte("key")}}}}
}
func TestDockerConfigCopiesProfilesAndAdvertisesBusyOrDrainedBase(t *testing.T) {
	c := dockerConfig()
	n, e := Normalize(c)
	if e != nil {
		t.Fatal(e)
	}
	c.Docker.Profiles[0].SSHHostKey[0] = 'X'
	c.Docker.Profiles[0].MemoryMiB = 42
	if n.Engine("standard").MemoryMiB != 8192 || string(n.Engine("standard").SSHHostKey) != "key" {
		t.Fatal("retained caller memory")
	}
	n.Slots[0].Count = 0
	if strings.Join(n.BaseCapabilities(n.Bases[0].Name), ",") != "docker,git" {
		t.Fatal("capacity affected discovery")
	}
	if strings.Join(n.Bases[0].Capabilities, ",") != "git" {
		t.Fatal("mutated ordinary capability facts")
	}
}
func TestDockerRejectsInvalidConfigurationBeforeWork(t *testing.T) {
	for _, change := range []func(*Config){func(c *Config) { c.Docker = nil }, func(c *Config) { c.Slots[0].DockerProfile = "missing" }, func(c *Config) { c.Docker.Profiles[0].Image = "ubuntu:latest" }, func(c *Config) { c.Docker.Profiles[0].DiskGiB = -1 }, func(c *Config) { c.Docker.Profiles[0].EngineVersion = "29;true" }, func(c *Config) { c.Bases[0].OS = "windows" }, func(c *Config) { c.Bases[0].OS = "linux"; c.Slots[0].DockerProfile = "" }, func(c *Config) { c.Bases[0].Capabilities = []string{"docker"} }} {
		c := dockerConfig()
		change(&c)
		if _, e := Normalize(c); e == nil {
			t.Fatal("invalid config accepted")
		}
	}
}

func TestSharedImageHostPinsCannotConflictOrDisappear(t *testing.T) {
	c := dockerConfig()
	c.Bases[0].Image = c.Docker.Profiles[0].Image
	c.Bases[0].SSHHostKey = []byte("base-pin")
	if _, e := Normalize(c); e == nil {
		t.Fatal("conflicting image host pins accepted")
	}
	c.Docker.Profiles[0].SSHHostKey = nil
	n, e := Normalize(c)
	if e != nil {
		t.Fatal(e)
	}
	if string(n.Docker.Profiles[0].SSHHostKey) != "base-pin" {
		t.Fatal("image pin silently dropped")
	}
}
