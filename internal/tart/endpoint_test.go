package tart

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func missingService(t *testing.T) error {
	t.Helper()
	return exec.Command("/bin/sh", "-c", "exit 113").Run()
}

func TestEndpointBootstrapFailureIsActionableAndArchived(t *testing.T) {
	b, _, _ := fixture(t)
	missing := missingService(t)
	calls := 0
	b.service = func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		switch args[0] {
		case "print":
			return nil, missing
		case "bootstrap":
			data, err := os.ReadFile(args[2])
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "<key>LimitLoadToSessionType</key><string>Background</string>") {
				t.Fatal("user-domain helper lacks Background session type")
			}
			return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
		}
		t.Fatalf("unexpected launchctl: %v", args)
		return nil, nil
	}
	ready, err := b.ensureEndpoint(t.Context(), "owned", "127.0.0.1:2222", 45100)
	if ready || !errors.Is(err, Err) || !strings.Contains(err.Error(), "Bootstrap failed: 5") {
		t.Fatal(ready, err)
	}
	dir := filepath.Join(b.dir, "endpoints", "owned")
	if calls != 2 {
		t.Fatal("unexpected service mutations", calls)
	}
	if e := os.WriteFile(filepath.Join(dir, "private-key"), []byte("do-not-export"), 0600); e != nil {
		t.Fatal(e)
	}
	b.service = func(context.Context, ...string) ([]byte, error) {
		return []byte("\tstate = waiting\n\tenvironment = {\n\t\tTOKEN = do-not-export\n\t\tstate = do-not-export\n\t}\n\tlast exit code = 78\n"), nil
	}
	diagnostics, err := b.EndpointDiagnostics(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(diagnostics)
	if strings.Contains(string(encoded), "do-not-export") || !strings.Contains(diagnostics["owned"]["last-error.log"], "Bootstrap failed: 5") {
		t.Fatal("diagnostics missing failure or leaked private data")
	}
}

func TestEndpointObservationFailureDoesNotBootstrapOrRelease(t *testing.T) {
	b, _, _ := fixture(t)
	missing := missingService(t)
	calls := []string{}
	b.service = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args[0])
		return nil, context.Canceled
	}
	if _, err := b.ensureEndpoint(t.Context(), "owned", "127.0.0.1:2222", 45100); err == nil {
		t.Fatal("observation failure treated as absence")
	}
	if err := b.RemoveEndpoint(t.Context(), "engine", "owned"); err == nil {
		t.Fatal("observation failure released helper")
	}
	if strings.Join(calls, ",") != "print,print" {
		t.Fatal(calls)
	}
	if _, err := os.Stat(filepath.Join(b.dir, "endpoints", "owned")); err != nil {
		t.Fatal("uncertain service state removed")
	}
	b.service = func(context.Context, ...string) ([]byte, error) { return nil, missing }
	if err := b.RemoveEndpoint(t.Context(), "engine", "owned"); err != nil {
		t.Fatal(err)
	}
}
