package tart

import (
	"reflect"
	"testing"
)

func TestDefaultBases(t *testing.T) {
	bases := DefaultBases()
	if len(bases) != 2 || bases[0].Name != "macos-26-arm64" || bases[0].OSVersion != "26.6.2" || bases[1].Name != "macos-15-arm64" || bases[1].OSVersion != "15.7.7" {
		t.Fatal("unexpected base catalog")
	}
	for _, base := range bases {
		c := testConfig(t)
		c.Bases = []Base{base}
		c.Slots[0].Base = base.Name
		if _, err := prepare(c); err != nil {
			t.Fatalf("invalid base %s: %v", base.Name, err)
		}
	}
	original := DefaultBases()
	bases[0].Name = "changed"
	bases[0].Capabilities[0] = "changed"
	if !reflect.DeepEqual(DefaultBases(), original) || bases[1].Capabilities[0] != "nix" {
		t.Fatal("shared mutable base catalog")
	}
}
