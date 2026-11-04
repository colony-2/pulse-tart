package tart

import (
	"embed"
	"github.com/colony-2/pulse-tart/internal/bundle"
)

//go:embed scripts/bootstrap-guest.sh scripts/install-guest.sh scripts/prepare-image.sh scripts/prepare-template.sh flake.nix flake.lock guest/*.go guest/go.mod.txt tests/integration/c2j-fixture.go.txt
var assets embed.FS

// GuestBundle returns the embedded preparation archive. Extract it in a fresh
// macOS VM and run sudo bash prepare-image.sh to produce a reusable base image.
func GuestBundle() ([]byte, error) { return bundle.Build(assets, "default") }
