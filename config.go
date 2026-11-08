package tart

import "github.com/colony-2/pulse-tart/internal/config"

// VMCount selects an explicit count, including zero. A nil count uses its default.
func VMCount(n int) *int { return &n }

// ResolveConfig fills conventions, validates configuration and returns an
// independent copy without contacting a backend or creating files or VMs.
// Constructors call it automatically. OwnerID and StateDir remain required.
// Omitted slots make every configured base usable at 4 vCPU / 8 GiB. Per-base
// template pools share the OS and total job limits; they do not add concurrency.
// Linux includes Docker; macOS Docker remains opt-in through Config.Docker.
func ResolveConfig(c Config) (Config, error) {
	counts, err := c.VMCounts.Normalize()
	if err != nil {
		return Config{}, err
	}
	c.VMCounts = counts
	if c.Bases == nil {
		c.Bases = append(DefaultBases(), DefaultLinuxBase())
	}
	if c.Slots == nil {
		macDocker := c.Docker != nil
		seen := map[string]bool{}
		for _, b := range c.Bases {
			os := b.OS
			if os == "" {
				os = "darwin"
			}
			g := SlotGroup{Name: "standard", Base: b.Name, Count: min(*counts.MacOS, *counts.Total)}
			if os == "linux" {
				g.Name = "linux"
				g.Count = min(*counts.Linux, *counts.Total)
				if seen[os] {
					g.Name = "linux-" + config.ImageToken(b.Name)
				}
			} else if seen[os] {
				g.Name = "mac-" + config.ImageToken(b.Name)
			}
			seen[os] = true
			if os == "linux" || macDocker {
				if c.Docker == nil {
					c.Docker = &DockerConfig{Profiles: []DockerEngineProfile{DefaultDockerEngine()}}
				} else if len(c.Docker.Profiles) == 0 {
					d := *c.Docker
					d.Profiles = []DockerEngineProfile{DefaultDockerEngine()}
					c.Docker = &d
				}
				g.DockerProfile = c.Docker.Profiles[0].Name
			}
			c.Slots = append(c.Slots, g)
		}
	}
	return config.Normalize(c)
}
