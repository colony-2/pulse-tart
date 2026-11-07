package tart

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse-tart/internal/config"
)

// No Tart or VM is needed. Exercise the real worker supervisor before expensive
// image builds, including Close/reopen, launchd restart and fixed-port cleanup.
func TestLiveDockerEndpoint(t *testing.T) {
	if os.Getenv("PULSE_TEST_DOCKER_ENDPOINT") != "1" {
		t.Skip("set PULSE_TEST_DOCKER_ENDPOINT=1 for the Mac launchd preflight")
	}
	if runtime.GOOS != "darwin" {
		t.Fatal("launchd preflight requires macOS")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	common := config.Config{StateDir: t.TempDir()}
	factory := func() *Client {
		b, err := open(config.Tart{}, common)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	b := factory()
	uidBytes := make([]byte, 16)
	if _, err := rand.Read(uidBytes); err != nil {
		t.Fatal(err)
	}
	uid := hex.EncodeToString(uidBytes)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "endpoint-ready") }))
	defer target.Close()
	address := strings.TrimPrefix(target.URL, "http://")
	reserve, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reserve.Addr().(*net.TCPAddr).Port
	reserve.Close()
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	var snapshot map[string]map[string]string
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		d, err := b.EndpointDiagnostics(cleanup)
		if err != nil {
			t.Errorf("endpoint diagnostics: %v", err)
		}
		if len(d) == 0 {
			d = snapshot
		}
		if directory := os.Getenv("PULSE_TEST_OUTPUT_DIR"); directory != "" {
			data, _ := json.MarshalIndent(d, "", "  ")
			if err := os.WriteFile(filepath.Join(directory, "endpoint-preflight.json"), append(data, '\n'), 0600); err != nil {
				t.Error(err)
			}
		}
		if err := b.RemoveEndpoint(cleanup, "preflight", uid); err != nil {
			t.Errorf("endpoint cleanup: %v", err)
		}
		if err := b.RemoveEndpoint(cleanup, "preflight", uid); err != nil {
			t.Errorf("repeated endpoint cleanup: %v", err)
		}
		b.Close()
	}()
	pid := func() int {
		var ready struct{ PID int }
		data, _ := os.ReadFile(filepath.Join(b.dir, "endpoints", uid, "ready.json"))
		json.Unmarshal(data, &ready)
		return ready.PID
	}
	wait := func(oldPID int) {
		t.Helper()
		for ctx.Err() == nil {
			ready, err := b.ensureEndpoint(ctx, uid, address, port)
			if err != nil {
				t.Fatal(err)
			}
			if ready && pid() != 0 && pid() != oldPID {
				response, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port))
				if err == nil {
					data, _ := io.ReadAll(response.Body)
					response.Body.Close()
					if string(data) == "endpoint-ready" {
						return
					}
				}
			}
			select {
			case <-ctx.Done():
			case <-time.After(100 * time.Millisecond):
			}
		}
		t.Fatal("worker endpoint did not become reachable", ctx.Err())
	}
	wait(0)
	original := pid()
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b = factory()
	wait(0)
	if pid() != original {
		t.Fatal("Close/reopen restarted the host service")
	}
	label := "user/" + strconv.Itoa(os.Getuid()) + "/" + b.endpointLabel(uid)
	if out, err := b.service(ctx, "kill", "SIGKILL", label); err != nil {
		t.Fatalf("restart injection: %v %s", err, out)
	}
	wait(original)
	snapshot, err = b.EndpointDiagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.RemoveEndpoint(ctx, "preflight", uid); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port)); err == nil {
		t.Fatal("endpoint still accepts requests after removal")
	}
	t.Log("DOCKER_ENDPOINT_SERVICE_PASS: Background launchd; forwarding; Close/reopen; same-port restart; removal")
}
