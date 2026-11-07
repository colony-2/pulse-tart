package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func relayFixture(t *testing.T, port int) (*Relay, *httptest.Server, *atomic.Bool) {
	t.Helper()
	running := &atomic.Bool{}
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		switch {
		case q.URL.Path == "/containers/json":
			if running.Load() {
				fmt.Fprintf(w, `[{"Ports":[{"PublicPort":%d,"Type":"tcp"}]}]`, port)
			} else {
				io.WriteString(w, "[]")
			}
		case strings.HasSuffix(q.URL.Path, "/start"):
			running.Store(true)
			w.WriteHeader(204)
		case strings.HasSuffix(q.URL.Path, "/stop"):
			running.Store(false)
			w.WriteHeader(204)
		case q.URL.Path == "/build":
			_ = http.NewResponseController(w).EnableFullDuplex()
			w.Header().Set("Content-Type", "application/json")
			io.Copy(w, q.Body)
		default:
			io.WriteString(w, "OK")
		}
	}))
	t.Cleanup(engine.Close)
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { io.WriteString(w, "ready") }))
	t.Cleanup(echo.Close)
	relay := NewRelay(t.Context(), func(ctx context.Context, network, address string) (net.Conn, error) {
		if network == "unix" {
			address = strings.TrimPrefix(engine.URL, "http://")
		} else {
			address = strings.TrimPrefix(echo.URL, "http://")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	})
	t.Cleanup(relay.Close)
	front := httptest.NewServer(relay.Handler())
	t.Cleanup(front.Close)
	return relay, front, running
}
func TestRelayStartGatesPublicationAndForwardsStreaming(t *testing.T) {
	l, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	_, front, _ := relayFixture(t, port)
	res, e := http.Post(front.URL+"/v1.52/containers/test/start", "application/json", nil)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 204 {
		t.Fatal(res.Status)
	}
	// No sleep/retry: a successful start guarantees the local listener exists.
	res, e = http.Get(fmt.Sprintf("http://127.0.0.1:%d", port))
	if e != nil {
		t.Fatal("start returned before forwarding was ready", e)
	}
	data, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(data) != "ready" {
		t.Fatal(string(data))
	}
	payload := strings.Repeat(`{"stream":"build output"}`+"\n", 10000)
	res, e = http.Post(front.URL+"/build", "application/x-tar", strings.NewReader(payload))
	if e != nil {
		t.Fatal(e)
	}
	data, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if string(data) != payload {
		t.Fatal("streaming response changed")
	}
}
func TestRelayConflictFailsStartAndStopsContainer(t *testing.T) {
	l, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	_, front, running := relayFixture(t, l.Addr().(*net.TCPAddr).Port)
	res, e := http.Post(front.URL+"/v1.52/containers/test/start", "application/json", nil)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 502 || running.Load() {
		t.Fatal("local conflict reported success or left container running")
	}
}
func TestRelayRecoversExistingPublications(t *testing.T) {
	l, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	relay, _, running := relayFixture(t, port)
	running.Store(true)
	if e = relay.Reconcile(t.Context()); e != nil {
		t.Fatal(e)
	}
	res, e := http.Get(fmt.Sprintf("http://127.0.0.1:%d", port))
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	running.Store(false)
	if e = relay.Reconcile(t.Context()); e != nil {
		t.Fatal(e)
	}
	// Verify closed binding without depending on HTTP connection pooling.
	c, e := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if e == nil {
		c.Close()
		t.Fatal("obsolete binding retained")
	}
}

func TestRelayStartIgnoresUnrelatedContainerConflict(t *testing.T) {
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port
	stopped := false
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		switch q.URL.Path {
		case "/containers/json":
			fmt.Fprintf(w, `[{"Ports":[{"PublicPort":%d,"Type":"tcp"}]}]`, port)
		case "/containers/new/start":
			w.WriteHeader(204)
		case "/containers/new/json":
			io.WriteString(w, `{"NetworkSettings":{"Ports":{}}}`)
		case "/containers/new/stop":
			stopped = true
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	}))
	defer engine.Close()
	relay := NewRelay(t.Context(), func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(engine.URL, "http://"))
	})
	defer relay.Close()
	front := httptest.NewServer(relay.Handler())
	defer front.Close()
	res, err := http.Post(front.URL+"/containers/new/start", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 204 || stopped {
		t.Fatal("unrelated port conflict stopped new container", res.StatusCode)
	}
}
