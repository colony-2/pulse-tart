package tart

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	return Config{OwnerID: "library", StateDir: dir, Bases: []Base{{Name: "macos-sequoia-15.7.7-arm64", Image: DefaultBases()[1].Image, OSVersion: "15.7.7", SSHPassword: "admin", Capabilities: []string{"git", "nix"}}}, Slots: []SlotGroup{{Name: "standard", Count: 1, Base: "macos-sequoia-15.7.7-arm64"}}}
}
func TestSharedConfigAndCopies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode([]any{}) }))
	defer server.Close()
	c := testConfig(t)
	connection := OrchardConfig{URL: server.URL, Worker: "mac", AllowInsecureHTTP: true, WorkerLabels: map[string]string{"rack": "one"}}
	r, e := NewOrchard(c, connection)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	if r.p.Config.Slots[0].CPU != 4 || r.p.Config.Slots[0].MemoryMiB != 8192 {
		t.Fatal("wrong defaults")
	}
	c.Bases[0].Capabilities[0] = "modified"
	c.Slots[0].Count = 0
	connection.WorkerLabels["rack"] = "modified"
	if r.p.Config.Bases[0].Capabilities[0] != "git" || r.p.Config.Slots[0].Count != 1 || r.p.Config.WorkerLabels["rack"] != "one" {
		t.Fatal("retained mutable config")
	}
	if e = r.Close(); e != nil {
		t.Fatal(e)
	}
	if e = r.Reconcile(context.Background()); e != ErrClosed {
		t.Fatal("closed runtime used")
	}
}
func TestEmbeddedBundleMatchesPreparationScript(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "bundle.tar.gz")
	cmd := exec.Command("bash", "scripts/build-bundle.sh", dest)
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("%v: %s", e, out)
	}
	unpack := func(b []byte) map[string]string {
		t.Helper()
		gz, e := gzip.NewReader(bytes.NewReader(b))
		if e != nil {
			t.Fatal(e)
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		files := map[string]string{}
		for {
			h, e := tr.Next()
			if e == io.EOF {
				break
			}
			if e != nil {
				t.Fatal(e)
			}
			data, e := io.ReadAll(tr)
			if e != nil {
				t.Fatal(e)
			}
			files[h.Name] = string(data)
		}
		return files
	}
	a, e := GuestBundle()
	if e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(dest)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(unpack(a), unpack(b)) {
		t.Fatal("library and image preparation environments differ")
	}
	if strings.Contains(string(a), "LEASE-SENTINEL") {
		t.Fatal("launch payload embedded")
	}
}
