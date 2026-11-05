package provider

import (
	"strings"
	"testing"

	"github.com/colony-2/pulse-tart/internal/config"
)

func TestBaseSelectorContract(t *testing.T) {
	for _, tc := range []struct {
		selector, name string
		want           bool
	}{
		{"macos-*arm64", "macos-sequoia-15.7.7-arm64", true},
		{"*sequoia*arm64", "macos-sequoia-15.7.7-arm64", true},
		{"macos**", "macos-sequoia-15.7.7-arm64", true},
		{"macos*", "macos", true},
		{"*", "macos", true},
		{"**", "macos", true},
		{"macos", "macos-sequoia-15.7.7-arm64", false},
		{"darwin*", "macos-sequoia-15.7.7-arm64", false},
		{"Macos*", "macos", false},
		{"*amd64", "macos-sequoia-15.7.7-arm64", false},
		{"mac*os", "macos-sequoia-15.7.7-arm64", false},
		{"a*a", "a", false},
		{"*ab*bc", "abc", false},
		{"*ab*bc", "ababc", true},
		{"org/*arm64", "org/macos/arm64", true},
		{"m?c[oa]s", "macos", false},
		{"m?c[oa]s", "m?c[oa]s", true},
		{`mac\*`, `mac\os`, true},
		{"*日本*", "macos-日本-arm64", true},
	} {
		t.Run(tc.selector+"/"+tc.name, func(t *testing.T) {
			l := launch("selector")
			l.Base = tc.selector
			if !validLaunch(l) {
				t.Fatal("valid selector rejected")
			}
			if got := baseMatches(config.Base{Name: tc.name}, l); got != tc.want {
				t.Fatalf("match = %v, want %v", got, tc.want)
			}
		})
	}
	for _, selector := range []string{"", "mac os", "mac\tos", "mac\nos", "mac\ros", "mac\x00os", "mac\xff", strings.Repeat("a", 4097)} {
		l := launch("invalid")
		l.Base = selector
		if validLaunch(l) {
			t.Errorf("invalid selector accepted: %q", selector)
		}
	}
	l := launch("limit")
	l.Base = strings.Repeat("a", 4096)
	if !validLaunch(l) {
		t.Fatal("4096-byte selector rejected")
	}
}
