package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Execute the actual shell probe. Commands model a Mac where only the worker
// can read its tools, and fail if a call uses the admin identity or omits sbin.
func TestMacOSPreflightRunsPrivateToolsAsWorker(t *testing.T) {
	dir := t.TempDir()
	scripts := map[string]string{
		"uname":   `case "$1" in -s) echo Darwin;; -m) echo arm64;; *) exit 1;; esac`,
		"sw_vers": `echo 26.6.2`,
		"sysctl":  `case "$2" in hw.ncpu) echo 4;; hw.memsize) echo 8589934592;; kern.bootsessionuuid) echo boot-id;; *) exit 1;; esac`,
		"sudo": `
[ "$1" = -u ] && [ "$2" = pulseworker ] || exit 90
shift 2
[ "$1" = env ] || exit 91
shift
[ "$1" = HOME=/Users/pulseworker ] || exit 92
shift
case "$1" in PATH=*:*/usr/sbin:*/sbin) ;; *) exit 93;; esac
shift
case "$1" in
 test) [ "$2" = -x ] && [ "$3" = /opt/pulse-docker/c2j ] || exit 94; echo worker_checked_c2j >>"$PROBE_LOG";;
 git) echo worker_checked_git >>"$PROBE_LOG";;
 docker)
   echo "worker_checked_$2" >>"$PROBE_LOG"
   if [ "$2" = buildx ] && [ "${FAIL_BUILDX:-}" = 1 ]; then echo PRIVATE_DATA_MUST_NOT_ESCAPE >&2; exit 17; fi
   if [ "$2" = info ] && [ "${3:-}" = --format ]; then echo engine-id; fi
   ;;
 *) exit 95;;
esac`,
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, fail := range []bool{false, true} {
		cmd := exec.Command("/bin/sh", "-c", macOSProbe(Config{Base: "macos-tahoe-26.6.2-arm64", CPU: 4, MemoryMiB: 7680}))
		cmd.Env = []string{"PATH=" + dir, "PROBE_LOG=" + filepath.Join(dir, "calls")}
		if fail {
			cmd.Env = append(cmd.Env, "FAIL_BUILDX=1")
		}
		output, err := cmd.CombinedOutput()
		if fail {
			if err == nil {
				t.Fatal("failing plugin passed admission")
			}
			problem := verificationFailure("Docker allocation verification failed", "PULSE_PREFLIGHT_FAILED=", string(output))
			if problem.Error() != "Docker allocation verification failed: buildx" {
				t.Fatal(problem)
			}
			if strings.Contains(problem.Error(), "PRIVATE_DATA") {
				t.Fatal("diagnostics leaked guest stderr")
			}
		} else {
			if err != nil || string(output) != "boot-id\nengine-id\n" {
				t.Fatal(string(output), err)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil || !strings.Contains(string(calls), "worker_checked_c2j") {
				t.Fatal(string(calls), err)
			}
		}
	}
}

func TestVerificationFailureDoesNotExposeArbitraryStderr(t *testing.T) {
	for _, output := range []string{"bearer-secret", "PULSE_PREFLIGHT_FAILED=oops secret=private", "PULSE_PREFLIGHT_FAILED=$(private)"} {
		problem := verificationFailure("failed", "PULSE_PREFLIGHT_FAILED=", output)
		if problem.Error() != "failed (guest transport or unclassified probe error)" {
			t.Fatal(problem)
		}
	}
	problem := verificationFailure("workspace failed", "PULSE_WORKSPACE_FAILED=", "PULSE_WORKSPACE_FAILED=PermissionError errno=13\n")
	if problem.Error() != "workspace failed: PermissionError errno=13" {
		t.Fatal(problem)
	}
}
