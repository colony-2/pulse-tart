package tart

// DefaultBases returns the pinned macOS ARM64 catalog, most preferred first.
// The public Cirrus Labs base images use admin/admin SSH credentials. Callers
// using their own images must set Image, OSVersion and credentials accordingly.
// Each call returns independent values. Add SlotGroups for the bases to enable;
// this helper does not create slots, download images or start VMs.
func DefaultBases() []Base {
	return []Base{
		{
			Name:         "macos-26-arm64",
			Image:        "ghcr.io/cirruslabs/macos-tahoe-base@sha256:4aaa751fa7538902d73c767f27a066fc87e83f183069da4e4b5022863abd4c77",
			OSVersion:    "26.6.2",
			SSHUsername:  "admin",
			SSHPassword:  "admin",
			Capabilities: []string{"nix", "git", "node", "pnpm", "uv", "python"},
		},
		{
			Name:         "macos-15-arm64",
			Image:        "ghcr.io/cirruslabs/macos-sequoia-base@sha256:eee659985b01e4df2a6c0754bd8d25e54969933c43fdcd4edef8bea337b3d354",
			OSVersion:    "15.7.7",
			SSHUsername:  "admin",
			SSHPassword:  "admin",
			Capabilities: []string{"nix", "git", "node", "pnpm", "uv", "python"},
		},
	}
}
