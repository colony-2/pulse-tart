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
	}
	resolved, err := tart.ResolveConfig(cfg)
	if err != nil {
		panic(err)
	}
	fmt.Println(*resolved.VMCounts.MacOS, *resolved.VMCounts.Linux, *resolved.VMCounts.Total, tart.DefaultCPU, tart.DefaultMemoryMiB)
	var _ api.Provider = (*tart.Runtime)(nil)
	// Output: 2 4 6 4 8192
}
