package tart_test

import (
	"fmt"
	api "github.com/colony-2/pulse-provider-api"
	tart "github.com/colony-2/pulse-tart"
)

func ExampleConfig() {
	// The same value is passed to NewOrchard or NewTart. The host obtains
	// credentials and paths through its own configuration mechanism.
	cfg := tart.Config{
		OwnerID: "pulse-macos", StateDir: "/var/lib/pulse/macos",
		Bases: tart.DefaultBases(),
		Slots: []tart.SlotGroup{{Name: "tahoe", Count: 2, Base: "macos-26-arm64"}},
	}
	fmt.Println(cfg.Slots[0].Count, tart.DefaultCPU, tart.DefaultMemoryMiB)
	var _ api.Provider = (*tart.Runtime)(nil)
	// Output: 2 4 8192
}
