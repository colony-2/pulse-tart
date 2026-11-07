package config

import (
	"crypto/sha256"
	"fmt"
	"regexp"
)

// Stable source names are independent of c2j and of provider journal keys.
// Unusual/long base labels get an unambiguous filesystem-safe token.
func ImageToken(value string) string {
	if len(value) <= 24 && regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`).MatchString(value) {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("id-%x", sum[:10])
}
func BaseImageName(owner, base string) string {
	return "pulse-" + ImageToken(owner) + "-" + ImageToken(base) + "-base"
}
func SlotImageName(owner string, g SlotGroup, index int) string {
	return fmt.Sprintf("pulse-%s-%s-%s-%dcpu-%dm-slot-%04d", ImageToken(owner), ImageToken(g.Base), ImageToken(g.Name), g.CPU, g.MemoryMiB, index)
}
