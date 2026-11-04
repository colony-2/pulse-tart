package tart_test

import (
	"fmt"
	api "github.com/colony-2/pulse-provider-api"
	tart "github.com/colony-2/pulse-tart"
)

func ExampleConfig() {
	// The same value is passed to NewOrchard or NewTart. The host obtains
	// credentials and paths through its own configuration mechanism.
	cfg := tart.Config{OwnerID: "pulse-macos", StateDir: "/var/lib/pulse/macos", Bases: []tart.Base{{Name: "macos-sequoia-15.7.7-arm64", Image: "ghcr.io/cirruslabs/macos-sequoia-base@sha256:eee659985b01e4df2a6c0754bd8d25e54969933c43fdcd4edef8bea337b3d354", OSVersion: "15.7.7", Aliases: []string{"macos", "darwin"}, SSHPassword: "image-password", Capabilities: []string{"nix", "git"}}}, Slots: []tart.SlotGroup{{Name: "standard", Count: 2, Base: "macos-sequoia-15.7.7-arm64"}}}
	fmt.Println(cfg.Slots[0].Count, tart.DefaultCPU, tart.DefaultMemoryMiB)
	var _ api.Provider = (*tart.Runtime)(nil)
	// Output: 2 4 8192
}
