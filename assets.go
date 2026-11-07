package tart

import (
	"embed"
)

//go:embed scripts/bootstrap-guest.sh scripts/install-guest.sh scripts/prepare-image.sh scripts/prepare-base.sh scripts/prepare-template.sh scripts/select-c2j.sh flake.nix flake.lock guest/*.go guest/go.mod.txt tests/integration/c2j-fixture.go.txt
var assets embed.FS
