package tart

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	api "github.com/colony-2/pulse-provider-api"
	orchardbackend "github.com/colony-2/pulse-tart/internal/orchard"
)

type qualificationOfflineTransport struct{}

func (qualificationOfflineTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("qualification: Orchard API connection unavailable")
}

func (q *qualification) orchardAPIOutage(pairs []DockerStatus, launches []api.Launch) {
	q.t.Helper()
	client := q.r.p.Backend.(*qualificationOrchardBackend).Client
	observer, err := orchardbackend.New(q.connection)
	if err != nil {
		q.t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(q.cfg.StateDir, "state.json"))
	if err != nil {
		q.t.Fatal(err)
	}
	transport := client.HTTP.Transport
	client.HTTP.Transport = qualificationOfflineTransport{}
	defer func() { client.HTTP.Transport = transport }()
	failures, probes := 0, 0
	start := time.Now()
	// The fault is scoped to this provider's HTTP transport. The independent
	// observer proves guest c2j/Docker/JobDB continue while the API is unavailable.
	waitLive(q.t, q.ctx, "Orchard API outage with continuing guest jobs", time.Minute, func(ctx context.Context) bool {
		if q.r.Reconcile(ctx) == nil {
			q.t.Fatal("API outage was hidden by reconciliation")
		}
		failures++
		if _, err := q.r.List(ctx, api.ListRequest{}); err == nil {
			q.t.Fatal("API outage returned a misleading inventory")
		}
		for _, p := range pairs {
			out, err := observer.Exec(ctx, p.Primary, p.PrimaryUID, []byte("curl --max-time 3 -fsS http://127.0.0.1:41081/marker\n"))
			if err != nil || string(out) != p.ID {
				q.t.Fatal("guest disrupted by API outage", err)
			}
			probes++
		}
		return time.Since(start) >= 30*time.Second
	})
	after, err := os.ReadFile(filepath.Join(q.cfg.StateDir, "state.json"))
	if err != nil || !bytes.Equal(before, after) {
		q.t.Fatal("unavailable inventory changed the allocation journal", err)
	}
	client.HTTP.Transport = transport
	if err := q.r.Reconcile(q.ctx); err != nil {
		q.t.Fatal("API recovery", err)
	}
	for _, launch := range launches {
		out, err := q.r.Submit(q.ctx, []api.Launch{launch})
		if err != nil || len(out) != 1 || out[0].Status != api.Accepted {
			q.t.Fatal("API recovery replay", err)
		}
	}
	for _, p := range pairs {
		q.healthy(p)
	}
	q.report.write(q.t, "orchard-api-outage.json", map[string]any{"seconds": time.Since(start).Seconds(), "failed_reconciliations": failures, "successful_guest_probes": probes, "journal_unchanged": true})
	q.checkpoint("orchard_api_outage")
}

func qualificationOrchardVM(ctx context.Context, c *orchardbackend.Client, connection OrchardConfig, name, uid string) (map[string]json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.Base+"/vms/"+url.PathEscape(name), nil)
	if err != nil {
		return nil, err
	}
	if connection.Token != "" {
		req.SetBasicAuth(connection.Username, string(connection.Token))
	}
	response, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("Orchard VM lookup status %d", response.StatusCode)
	}
	var vm map[string]json.RawMessage
	if err = json.NewDecoder(io.LimitReader(response.Body, orchardbackend.MaxBody)).Decode(&vm); err != nil {
		return nil, err
	}
	var got string
	if json.Unmarshal(vm["uid"], &got) != nil || got != uid {
		return nil, errors.New("Orchard fault target UID mismatch")
	}
	return vm, nil
}
func qualificationPutOrchardVM(ctx context.Context, c *orchardbackend.Client, connection OrchardConfig, name string, vm map[string]json.RawMessage) error {
	body, err := json.Marshal(vm)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "PUT", c.Base+"/vms/"+url.PathEscape(name), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if connection.Token != "" {
		req.SetBasicAuth(connection.Username, string(connection.Token))
	}
	response, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Orchard VM update status %d", response.StatusCode)
	}
	return nil
}
func (q *qualification) orchardEndpointRecovery(p, peer DockerStatus) {
	q.t.Helper()
	c := q.r.p.Backend.(*qualificationOrchardBackend).Client
	vm, err := qualificationOrchardVM(q.ctx, c, q.connection, p.Engine, p.EngineUID)
	if err != nil {
		q.t.Fatal(err)
	}
	var endpoints []map[string]any
	if err = json.Unmarshal(vm["endpoints"], &endpoints); err != nil {
		q.t.Fatal(err)
	}
	kept := []map[string]any{}
	var original map[string]any
	for _, endpoint := range endpoints {
		if endpoint["name"] == "pulse-engine-ssh" {
			original = endpoint
		} else {
			kept = append(kept, endpoint)
		}
	}
	if original == nil {
		q.t.Fatal("missing endpoint before fault")
	}
	port := int(original["workerPortRange"].(map[string]any)["min"].(float64))
	vm["endpoints"], _ = json.Marshal(kept)
	if err = qualificationPutOrchardVM(q.ctx, c, q.connection, p.Engine, vm); err != nil {
		q.t.Fatal(err)
	}
	// Always restore only the removed endpoint, preserving unrelated current fields.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		current, e := qualificationOrchardVM(ctx, c, q.connection, p.Engine, p.EngineUID)
		if e != nil {
			return
		}
		var eps []map[string]any
		_ = json.Unmarshal(current["endpoints"], &eps)
		for _, ep := range eps {
			if ep["name"] == "pulse-engine-ssh" {
				return
			}
		}
		current["endpoints"], _ = json.Marshal(append(eps, original))
		if e = qualificationPutOrchardVM(ctx, c, q.connection, p.Engine, current); e != nil {
			q.t.Error("restore faulted endpoint", e)
		}
	}()
	waitLive(q.t, q.ctx, "Orchard endpoint withdrawal", 30*time.Second, func(ctx context.Context) bool {
		current, err := qualificationOrchardVM(ctx, c, q.connection, p.Engine, p.EngineUID)
		if err != nil {
			q.t.Fatal(err)
		}
		var observed []struct{ Name, State string }
		_ = json.Unmarshal(current["observedEndpoints"], &observed)
		for _, ep := range observed {
			if ep.Name == "pulse-engine-ssh" && ep.State == "listening" {
				return false
			}
		}
		_, err = q.execContext(ctx, p, false, "curl --max-time 3 -fsS http://127.0.0.1:41081/marker")
		return err != nil
	})
	q.healthy(peer)
	waitLive(q.t, q.ctx, "Orchard endpoint restoration on the original port", time.Minute, func(ctx context.Context) bool {
		if err := q.r.Reconcile(ctx); err != nil {
			q.t.Log("endpoint recovery", err)
			return false
		}
		ready, err := c.Endpoint(ctx, p.Engine, p.EngineUID, port)
		if err != nil || !ready {
			return false
		}
		out, err := q.execContext(ctx, p, false, "curl --max-time 3 -fsS http://127.0.0.1:41081/marker")
		return err == nil && string(out) == p.ID
	})
	q.healthy(peer)
	q.report.write(q.t, "orchard-endpoint-recreation.json", map[string]any{"uid": p.EngineUID, "port": port, "withdrawal_observed": true, "same_port_recovered": true})
	q.checkpoint("orchard_endpoint_recreation")
}

func TestOrchardQualificationMutationChecksUIDAndPreservesFields(t *testing.T) {
	puts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "fixture" || pass != "test-token" {
			t.Error("authentication missing")
			w.WriteHeader(401)
			return
		}
		if r.Method == "GET" {
			fmt.Fprint(w, `{"uid":"owned","unknownField":{"keep":true},"endpoints":[]}`)
			return
		}
		puts++
		var vm map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&vm) != nil || string(vm["unknownField"]) != `{"keep":true}` {
			t.Error("mutation lost unrelated VM fields")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	cfg := OrchardConfig{URL: server.URL, AllowInsecureHTTP: true, Username: "fixture", Token: "test-token"}
	c, err := orchardbackend.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = qualificationOrchardVM(t.Context(), c, cfg, "engine", "foreign"); err == nil {
		t.Fatal("UID mismatch accepted")
	}
	if puts != 0 {
		t.Fatal("mismatched UID mutated VM")
	}
	vm, err := qualificationOrchardVM(t.Context(), c, cfg, "engine", "owned")
	if err != nil {
		t.Fatal(err)
	}
	if err = qualificationPutOrchardVM(t.Context(), c, cfg, "engine", vm); err != nil {
		t.Fatal(err)
	}
	if puts != 1 {
		t.Fatal("missing owned mutation")
	}
}
