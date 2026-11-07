package orchard

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"github.com/colony-2/pulse-tart/internal/backend"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/websocket"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDockerEndpointPinsPortAndPreservesVMFields(t *testing.T) {
	object := map[string]any{"uid": "owned", "powerState": "running", "unknownField": "keep", "endpoints": []any{map[string]any{"name": "other", "protocol": "tcp", "target": map[string]any{"vm": map[string]any{"port": 8080}}}}}
	puts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			puts++
			if e := json.NewDecoder(r.Body).Decode(&object); e != nil {
				t.Fatal(e)
			}
		}
		json.NewEncoder(w).Encode(object)
	}))
	defer server.Close()
	c := &Client{Base: server.URL, HTTP: server.Client()}
	ready, e := c.Endpoint(context.Background(), "engine", "owned", 45101)
	if e != nil || ready || puts != 1 || object["unknownField"] != "keep" {
		t.Fatal(ready, e, object)
	}
	endpoints := object["endpoints"].([]any)
	if len(endpoints) != 2 {
		t.Fatal("unrelated endpoint removed")
	}
	endpoint := endpoints[1].(map[string]any)
	ports := endpoint["workerPortRange"].(map[string]any)
	if ports["min"] != float64(45101) || ports["max"] != float64(45101) {
		t.Fatal(endpoint)
	}
	object["observedEndpoints"] = []any{map[string]any{"name": "pulse-engine-ssh", "state": "listening", "workerPort": 45101}}
	ready, e = c.Endpoint(context.Background(), "engine", "owned", 45101)
	if e != nil || !ready || puts != 1 {
		t.Fatal(ready, e)
	}
	if _, e = c.Endpoint(context.Background(), "engine", "foreign", 45101); e == nil {
		t.Fatal("foreign UID accepted")
	}
	if _, e = c.Endpoint(context.Background(), "engine", "owned", 45102); e == nil || puts != 1 {
		t.Fatal("silently changed existing endpoint port")
	}
	endpoint["target"] = map[string]any{"vm": map[string]any{"port": 22}}
	if _, e = c.Endpoint(context.Background(), "engine", "owned", 45101); e == nil {
		t.Fatal("changed target accepted")
	}
}

func TestGuestHostKeyFollowsPreparedSourceAncestry(t *testing.T) {
	keys := map[string][]byte{"raw": []byte("pinned"), "other": nil}
	sources := []backend.VM{{Spec: backend.Spec{Image: "raw"}, LocalName: "base"}, {Spec: backend.Spec{Image: "base"}, LocalName: "slot"}}
	key, err := inheritedGuestKey(keys, sources, "slot")
	if err != nil || string(key) != "pinned" {
		t.Fatal("clone lost configured pin", err)
	}
	if _, err = inheritedGuestKey(keys, sources, "unproven"); err == nil {
		t.Fatal("unproven source silently unpinned")
	}
	sources[0].Image = "slot"
	if _, err = inheritedGuestKey(keys, sources, "slot"); err == nil {
		t.Fatal("source cycle accepted")
	}
}

func TestManagementSSHUsesBinaryWebsocketAndStdin(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	sshConfig := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) { return nil, nil }}
	sshConfig.AddHostKey(signer)
	received := make(chan string, 1)
	handler := http.NewServeMux()
	handler.HandleFunc("/vms/vm", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(backend.VM{Spec: backend.Spec{Name: "vm", Image: "raw", Username: "admin", Password: "private-password"}, UID: "owned"})
	})
	handler.Handle("/vms/vm/port-forward", websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		if ws.Request().Header.Get("User-Agent") == "" {
			t.Error("Orchard switches to text frames without a User-Agent")
			return
		}
		ws.PayloadType = websocket.BinaryFrame
		conn, channels, requests, err := ssh.NewServerConn(ws, sshConfig)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		go ssh.DiscardRequests(requests)
		channel := <-channels
		ch, reqs, err := channel.Accept()
		if err != nil {
			t.Error(err)
			return
		}
		defer ch.Close()
		req := <-reqs
		var command struct{ Command string }
		ssh.Unmarshal(req.Payload, &command)
		if req.Type != "exec" || command.Command != "/bin/bash -s" {
			t.Error("payload escaped stdin")
		}
		req.Reply(true, nil)
		payload, _ := io.ReadAll(ch)
		received <- string(payload)
		ch.Write([]byte("ready\n"))
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
	}))
	server := httptest.NewServer(handler)
	defer server.Close()
	c := &Client{Base: server.URL, HTTP: server.Client(), GuestKeys: map[string][]byte{"raw": ssh.MarshalAuthorizedKey(signer.PublicKey())}}
	out, err := c.Exec(t.Context(), "vm", "owned", []byte("private stdin payload"))
	if err != nil || string(out) != "ready\n" {
		t.Fatal(string(out), err)
	}
	if <-received != "private stdin payload" {
		t.Fatal("stdin changed")
	}
}
