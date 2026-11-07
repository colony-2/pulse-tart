package releases

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReleaseResolution(t *testing.T) {
	sha := strings.Repeat("a", 40)
	calls := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		switch r.URL.Path {
		case "/releases/latest":
			fmt.Fprint(w, `{"tag_name":"v1.2.3"}`)
		case "/commits/v1.2.3":
			fmt.Fprintf(w, `{"sha":%q}`, sha)
		default:
			http.Error(w, "missing", 404)
		}
	}))
	defer server.Close()
	revision, err := resolve(t.Context(), server.Client(), server.URL, "latest")
	if err != nil || revision != sha || strings.Join(calls, ",") != "/releases/latest,/commits/v1.2.3" {
		t.Fatal(revision, err, calls)
	}
	calls = nil
	if _, err = resolve(t.Context(), server.Client(), server.URL, sha); err != nil || len(calls) != 0 {
		t.Fatal("commit lookup should require no network", err)
	}
	if _, err = resolve(t.Context(), server.Client(), server.URL, "missing"); err == nil {
		t.Fatal("missing release silently accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = resolve(ctx, server.Client(), server.URL, "latest"); err == nil {
		t.Fatal("canceled release lookup succeeded")
	}
}
