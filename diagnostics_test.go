package tart

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	api "github.com/colony-2/pulse-provider-api"
)

func TestReportRequiresCompleteJobDBCoverage(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "python3", "-B", "scripts/test_report_test.py")
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("report contract: %v\n%s", e, out)
	}
}
func TestBundleVersionSelection(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "python3", "-B", "scripts/test_bundle_test.py")
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("bundle selection: %v\n%s", e, out)
	}
}
func TestDiagnosticRedactionPreservesJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PULSE_TEST_OUTPUT_DIR", dir)
	password := "password-with-quote\"\n"
	report := newLiveReport(t, password)
	lease := api.Lease(`{"leaseToken":"fixture-bearer-value","jobId":"job"}`)
	report.addLease(lease)
	log := string(lease) + " " + "fixture-bearer-value" + " " + base64.StdEncoding.EncodeToString([]byte(lease)) + " " + password
	report.write(t, "report.json", map[string]any{"logs": []any{log}, "count": 3})
	data, e := os.ReadFile(filepath.Join(dir, "report.json"))
	if e != nil {
		t.Fatal(e)
	}
	var got map[string]any
	if json.Unmarshal(data, &got) != nil {
		t.Fatal("redaction corrupted report JSON")
	}
	if strings.Contains(string(data), "fixture-bearer-value") || strings.Contains(string(data), "password-with-quote") || strings.Contains(string(data), base64.StdEncoding.EncodeToString([]byte(lease))) {
		t.Fatal("diagnostic leaked credentials")
	}
	if got["count"] != float64(3) {
		t.Fatal("diagnostic values changed")
	}
}

func TestStandaloneImageUpdater(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "python3", "-B", "scripts/test_update_images_test.py")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("image updater: %v\n%s", err, out)
	}
}
