package tart

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse-tart/internal/backend"
)

// Keep one pre-update clone running while the standalone script replaces all
// sources. At most two guests run: that clone plus one builder/new clone.
func exerciseImageUpdate(t *testing.T, ctx context.Context, r *Runtime, owner, stateDir string, base Base, version string, report *liveReport) {
	t.Helper()
	slots, err := r.Slots()
	if err != nil || len(slots) != 2 {
		t.Fatal("image update needs two prepared slots")
	}
	source, err := r.p.Backend.Get(ctx, slots[0].Template)
	if err != nil {
		t.Fatal(err)
	}
	clone := func(suffix string) backend.VM {
		t.Helper()
		spec := source.Spec
		spec.Name = "pulse-update-" + owner + "-" + suffix
		spec.Image = source.LocalName
		spec.StartupScript = backend.Script{Content: "#!/bin/bash\necho PULSE_UPDATE_PROBE_READY\n", Env: map[string]string{}}
		intent := fixtureSpecBytes(spec)
		if err := os.WriteFile(filepath.Join(stateDir, "update-"+suffix+"-intent.json"), intent, 0600); err != nil {
			t.Fatal(err)
		}
		created := backend.VM{Spec: spec}
		report.extra[spec.Name] = ""
		// Function-scoped cleanup, before JobDB needs both slots again.
		return created
	}
	probes := []backend.VM{}
	defer func() {
		clean, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		report.capture(t, clean, "tart", OrchardConfig{}, r, stateDir)
		for _, probe := range probes {
			removed := false
			for clean.Err() == nil {
				v, err := r.p.Backend.Get(clean, probe.Name)
				if backend.IsStatus(err, 404) {
					removed = true
					break
				}
				if err != nil || (probe.UID != "" && probe.UID != v.UID) || !bytes.Equal(fixtureSpecBytes(probe.Spec), fixtureSpecBytes(v.Spec)) {
					t.Error("image update probe cleanup ownership check failed")
					break
				}
				if !v.Stopped() {
					_ = r.p.Backend.(interface {
						Suspend(context.Context, string, string) error
					}).Suspend(clean, v.Name, v.UID)
				} else {
					_ = r.p.Backend.Delete(clean, v.Name)
				}
				time.Sleep(time.Second)
			}
			if !removed {
				t.Error("image update probe cleanup incomplete; preserve private intent")
			}
		}
	}()
	start := func(suffix string) backend.VM {
		t.Helper()
		probe := clone(suffix)
		probes = append(probes, probe) // Save intent even when dispatch is uncertain.
		created, err := r.p.Backend.Create(ctx, probe.Spec)
		if err != nil {
			t.Fatal("image update probe create uncertain; preserve private intent")
		}
		probes[len(probes)-1] = created
		report.extra[created.Name] = created.UID
		return created
	}
	identity := func(v backend.VM) string {
		t.Helper()
		var identity string
		waitLive(t, ctx, "image update probe identity: "+v.Name, 3*time.Minute, func(ctx context.Context) bool {
			out, err := testGuestExec(ctx, "tart", OrchardConfig{}, v, "/usr/sbin/sysctl -n kern.bootsessionuuid; /sbin/ifconfig en0 | /usr/bin/awk '/ether / {print $2}'; /opt/pulse-tart/bin/c2j version")
			if err != nil || len(strings.Split(strings.TrimSpace(string(out)), "\n")) < 3 {
				return false
			}
			identity = strings.TrimSpace(string(out))
			return true
		})
		return identity
	}
	old := start("old")
	before := identity(old)
	if version == "" {
		version = "latest"
	}
	updateCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(updateCtx, "bash", "scripts/update-macos-images.sh", "--owner", owner, "--base", base.Name, "--group", "standard", "--slots", "2", "--cpu", "4", "--memory", "8192", "--image", base.Image, "--os-version", base.OSVersion, "--ssh-user", base.SSHUsername, "--c2j", version)
	cmd.Env = append(os.Environ(), "PULSE_IMAGE_PROFILE=integration", "PULSE_IMAGE_SSH_PASSWORD="+string(base.SSHPassword))
	home := os.Getenv("PULSE_TEST_TART_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		home = filepath.Join(userHome, ".tart")
	}
	cmd.Env = append(cmd.Env, "TART_HOME="+home)
	if report.dir != "" {
		cmd.Env = append(cmd.Env, "PULSE_IMAGE_LOG_DIR="+filepath.Join(report.dir, "image-update"))
	}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	exited := false
	defer func() {
		if !exited {
			cancel()
			<-finished
		}
	}()
	waitLive(t, updateCtx, "standalone image update with old clone running", 30*time.Minute, func(context.Context) bool {
		select {
		case err := <-finished:
			exited = true
			report.write(t, "image-update.json", map[string]any{"output": output.String(), "passed": err == nil})
			if err != nil {
				t.Fatalf("standalone image update failed: %v (see image-update.json)", err)
			}
			return true
		default:
			return false
		}
	})
	if after := identity(old); before != after {
		t.Fatal("updating the source changed the running clone's boot identity or c2j version")
	}
	fresh := start("new")
	after := identity(fresh)
	oldLines, newLines := strings.Split(before, "\n"), strings.Split(after, "\n")
	if oldLines[0] == newLines[0] || oldLines[1] == newLines[1] {
		t.Fatal("replacement did not acquire a fresh boot identity and MAC")
	}
	// Compare the new clone's identity with its own saved readiness file. This
	// fails if Tart cold-booted rather than restoring the replacement snapshot.
	out, err := testGuestExec(ctx, "tart", OrchardConfig{}, fresh, "sudo cat /var/db/pulse-tart-template/ready")
	if err != nil || !strings.Contains(string(out), newLines[0]) || !strings.Contains(string(out), newLines[1]) {
		t.Fatal("replacement clone did not restore its template identity")
	}
	report.write(t, "image-update-identities.json", map[string]string{"old": before, "replacement": after})
	t.Log("IMAGE_UPDATE_PASS: standalone publication preserved running clone; new clone restored replacement with fresh boot UUID and MAC")
}
