package config

import (
	"bytes"
	"net"
	"regexp"
	"slices"
	"time"
)

// Docker enables isolated engines on explicitly selected slot groups.
// The image must be an ARM64 Ubuntu 24.04 Tart disk, never a container image.
type Docker struct {
	Profiles     []EngineProfile
	ReadyTimeout time.Duration
}
type EngineProfile struct {
	Name          string
	Image         string
	SSHUsername   string
	SSHPassword   Secret
	SSHHostKey    []byte
	CPU           int64
	MemoryMiB     int64
	DiskGiB       int64 // Bounded private ext4 filesystem for Docker's writable data.
	EngineVersion string
}

// Endpoint is worker connectivity, not a job-visible Docker address. Ports must
// be reserved for this owner on this worker; listeners expose key-only SSH.
type Endpoint struct {
	Address          string
	PortMin, PortMax int
}

func (e Endpoint) Validate() error {
	if e.Address == "" || net.ParseIP(e.Address).To4() == nil || e.PortMin < 1024 || e.PortMax > 65535 || e.PortMax < e.PortMin {
		return Err
	}
	return nil
}
func normalizeDocker(c *Config) error {
	profiles := map[string]bool{}
	keys := map[string][]byte{}
	for _, base := range c.Bases {
		if old := keys[base.Image]; len(old) > 0 && len(base.SSHHostKey) > 0 && !bytes.Equal(old, base.SSHHostKey) {
			return Err
		}
		if len(base.SSHHostKey) > 0 {
			keys[base.Image] = base.SSHHostKey
		}
	}
	if c.Docker != nil {
		d := *c.Docker
		c.Docker = &d
		d.Profiles = slices.Clone(d.Profiles)
		if d.ReadyTimeout == 0 {
			d.ReadyTimeout = 15 * time.Minute
		}
		if d.ReadyTimeout < time.Minute || d.ReadyTimeout > 2*time.Hour || len(d.Profiles) == 0 {
			return Err
		}
		for i := range d.Profiles {
			p := &d.Profiles[i]
			if !Name(p.Name) || profiles[p.Name] || !digestImage.MatchString(p.Image) || p.SSHPassword == "" {
				return Err
			}
			profiles[p.Name] = true
			if p.SSHUsername == "" {
				p.SSHUsername = "admin"
			}
			if !Name(p.SSHUsername) {
				return Err
			}
			if old := keys[p.Image]; len(old) > 0 && len(p.SSHHostKey) > 0 && !bytes.Equal(old, p.SSHHostKey) {
				return Err
			}
			if len(p.SSHHostKey) == 0 {
				p.SSHHostKey = keys[p.Image]
			}
			p.SSHHostKey = slices.Clone(p.SSHHostKey)
			if len(p.SSHHostKey) > 0 {
				keys[p.Image] = p.SSHHostKey
			}
			if p.CPU == 0 {
				p.CPU = 4
			}
			if p.MemoryMiB == 0 {
				p.MemoryMiB = 8192
			}
			if p.DiskGiB == 0 {
				p.DiskGiB = 32
			}
			if p.EngineVersion == "" {
				p.EngineVersion = "29.1.3"
			}
			if p.CPU < 1 || p.CPU > 256 || p.MemoryMiB < 2048 || p.MemoryMiB > 1048576 || p.DiskGiB < 8 || p.DiskGiB > 1024 || !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(p.EngineVersion) {
				return Err
			}
		}
		*c.Docker = d
	}
	for i := range c.Bases {
		c.Bases[i].SSHHostKey = slices.Clone(keys[c.Bases[i].Image])
	}
	if c.Docker != nil {
		for i := range c.Docker.Profiles {
			c.Docker.Profiles[i].SSHHostKey = slices.Clone(keys[c.Docker.Profiles[i].Image])
		}
	}
	for _, g := range c.Slots {
		if g.DockerProfile != "" && (!profiles[g.DockerProfile] || g.MemoryMiB < 2048) {
			return Err
		}
		for _, b := range c.Bases {
			if b.Name == g.Base && b.OS == "linux" && (g.DockerProfile == "" || b.OSVersion != "24.04") {
				return Err
			}
		}
	}
	return nil
}
func (c Config) Engine(name string) EngineProfile {
	if c.Docker != nil {
		for _, p := range c.Docker.Profiles {
			if p.Name == name {
				return p
			}
		}
	}
	return EngineProfile{}
}
func (c Config) BaseCapabilities(name string) []string {
	var caps []string
	for _, b := range c.Bases {
		if b.Name == name {
			caps = slices.Clone(b.Capabilities)
		}
	}
	for _, g := range c.Slots {
		if g.Base == name && g.DockerProfile != "" {
			caps = append(caps, "docker")
			break
		}
	}
	slices.Sort(caps)
	return slices.Compact(caps)
}
