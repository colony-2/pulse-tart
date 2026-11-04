package orchard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/colony-2/pulse-tart/internal/config"
)

func TestHTTPBoundaries(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 409, 429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/prefix/v1/vms" {
					t.Error(r.URL.Path)
				}
				w.WriteHeader(status)
				io.WriteString(w, "secret backend error")
			}))
			defer s.Close()
			c, e := New(config.Orchard{URL: s.URL + "/prefix/v1/", AllowInsecureHTTP: true})
			if e != nil {
				t.Fatal(e)
			}
			_, e = c.List(t.Context())
			if !IsStatus(e, status) {
				t.Fatal(e)
			}
		})
	}
	for _, body := range []string{"null", "{}", "[", `[] junk`} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		c, _ := New(config.Orchard{URL: s.URL, AllowInsecureHTTP: true})
		if _, e := c.List(t.Context()); e == nil {
			t.Error("accepted", body)
		}
		s.Close()
	}
}
func TestBasicAndRedirect(t *testing.T) {
	t.Setenv("ORCHARD_TEST_TOKEN", "sentinel")
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redirect followed") }))
	defer dst.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "service" || p != "sentinel" {
			t.Error("auth missing")
		}
		http.Redirect(w, r, dst.URL, 307)
	}))
	defer s.Close()
	c, e := New(config.Orchard{URL: s.URL, AllowInsecureHTTP: true, Username: "service", Token: "sentinel"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.List(t.Context()); !IsStatus(e, 307) {
		t.Fatal(e)
	}
}
func TestTLSAndCancellation(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "[]") }))
	defer s.Close()
	c, _ := New(config.Orchard{URL: s.URL})
	if _, e := c.List(t.Context()); e == nil {
		t.Fatal("untrusted TLS accepted")
	}
	c.HTTP = s.Client()
	if _, e := c.List(t.Context()); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	if _, e := c.List(ctx); e == nil {
		t.Fatal("cancellation ignored")
	}
}

func TestSuspendPreservesUnknownFieldsAndUID(t *testing.T) {
	var puts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			fmt.Fprint(w, `{"uid":"owned","powerState":"running","os":"darwin","futureField":{"keep":true}}`)
			return
		}
		puts++
		var got map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		if string(got["powerState"]) != `"suspended"` || string(got["futureField"]) != `{"keep":true}` {
			t.Error("partial PUT", got)
		}
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	c, err := New(config.Orchard{URL: server.URL, AllowInsecureHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Suspend(t.Context(), "name", "replacement"); err == nil || puts != 0 {
		t.Fatal("UID mismatch wrote")
	}
	if err = c.Suspend(t.Context(), "name", "owned"); err != nil || puts != 1 {
		t.Fatal(err, puts)
	}
}
