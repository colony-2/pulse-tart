package tart

import "github.com/colony-2/pulse-tart/internal/config"

// VMCount selects an explicit count, including zero. A nil count uses its default.
func VMCount(n int) *int { return &n }

// ResolveConfig fills conventions, validates configuration and returns an
// independent copy without contacting a backend or creating files or VMs.
// Constructors call it automatically. OwnerID and StateDir remain required.
// Omitted slots use the first configured base for each OS at 4 vCPU / 8 GiB.
// Linux includes Docker; macOS Docker remains opt-in through Config.Docker.
func ResolveConfig(c Config) (Config, error) {
	counts, err := c.VMCounts.Normalize()
	if err != nil {
		return Config{}, err
	}
	c.VMCounts = counts
	if c.Bases == nil {
		c.Bases = []Base{DefaultBases()[0], DefaultLinuxBase()}
	}
	if c.Slots == nil {
		macDocker := c.Docker != nil
		seen := map[string]bool{}
		for _, b := range c.Bases {
			os := b.OS
			if os == "" {
				os = "darwin"
			}
			if seen[os] {
				continue
			}
			seen[os] = true
			g := SlotGroup{Name: "standard", Base: b.Name, Count: min(*counts.MacOS, *counts.Total)}
			if os == "linux" {
				g.Name = "linux"
				g.Count = min(*counts.Linux, *counts.Total)
			}
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
