package config

import (
	"os/exec"
	"strings"
	"testing"
)

func TestScriptAndProviderNamesMatch(t *testing.T) {
	for _, base := range []string{"macos-26-arm64", "custom/base/with-a-very-long-name", "日本語"} {
		g := SlotGroup{Name: "standard", Base: base, CPU: 4, MemoryMiB: 8192}
		expected := BaseImageName("test", base) + "\n" + SlotImageName("test", g, 1) + "\n" + SlotImageName("test", g, 2) + "\n"
		out, err := exec.Command("python3", "../../scripts/image-names.py", "test", base, "standard", "4", "8192", "2").CombinedOutput()
		if err != nil || string(out) != expected {
			t.Fatal(string(out), expected, err)
		}
		for _, name := range strings.Fields(expected) {
			if !Name(name) {
				t.Fatal("invalid source name", name)
			}
		}
	}
}
