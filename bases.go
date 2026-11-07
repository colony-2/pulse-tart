package tart

import (
	_ "embed"
	"encoding/json"
)

//go:embed bases.json
var baseCatalog []byte

// DefaultBases returns independent macOS ARM64 configurations, most preferred
// first. This OS catalog is shared with the local image preparation script.
// c2j releases are resolved at preparation time, independently of this library.
func DefaultBases() []Base {
	var bases []Base
	if err := json.Unmarshal(baseCatalog, &bases); err != nil {
		panic("invalid embedded base catalog")
	}
	return bases
}
