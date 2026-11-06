package docker

import (
	"fmt"
	"regexp"
	"strings"
)

// All guest tools run with the same identity and environment as the executor.
// In particular, admin cannot traverse the worker-owned 0700 tool directory.
const macWorker = "sudo -u pulseworker env HOME=/Users/pulseworker PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

func macOSProbe(c Config) string {
	version := strings.TrimSuffix(strings.TrimPrefix(c.Base, "macos-tahoe-"), "-arm64")
	return fmt.Sprintf(`check() {
 label=$1
 shift
 if ! "$@" >/dev/null; then
   echo "PULSE_PREFLIGHT_FAILED=$label" >&2
   exit 1
 fi
}
check platform test "$(uname -s)/$(uname -m)" = Darwin/arm64
check os_version test "$(sw_vers -productVersion)" = %s
check cpu test "$(sysctl -n hw.ncpu)" -ge %d
check memory test "$(sysctl -n hw.memsize)" -ge %d
check c2j_executable %s test -x /opt/pulse-docker/c2j
check git %s git --version
check docker_info %s docker info
check buildx %s docker buildx version
check compose %s docker compose version
boot=$(sysctl -n kern.bootsessionuuid) || { echo PULSE_PREFLIGHT_FAILED=boot_identity >&2; exit 1; }
engine=$(%s docker info --format '{{.ID}}') || { echo PULSE_PREFLIGHT_FAILED=engine_identity >&2; exit 1; }
printf '%%s\n%%s\n' "$boot" "$engine"
`, version, c.CPU, c.MemoryMiB<<20, macWorker, macWorker, macWorker, macWorker, macWorker, macWorker)
}

// Return only diagnostic codes, never arbitrary guest stderr or credentials.
func verificationFailure(message, marker, stderr string) error {
	for _, line := range strings.Split(stderr, "\n") {
		if !strings.HasPrefix(line, marker) {
			continue
		}
		code := strings.TrimPrefix(line, marker)
		if regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}( errno=[0-9]+)?$`).MatchString(code) {
			return fmt.Errorf("%s: %s", message, code)
		}
	}
	return fmt.Errorf("%s (guest transport or unclassified probe error)", message)
}
