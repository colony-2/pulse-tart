package orchard

import (
	"context"
	"encoding/json"
	"github.com/colony-2/pulse-tart/internal/backend"
	"github.com/colony-2/pulse-tart/internal/guestconn"
	"golang.org/x/net/websocket"
	"net"
	"net/http"
	"net/url"
)

func (c *Client) Exec(ctx context.Context, name, uid string, script []byte) ([]byte, error) {
	v, e := c.Get(ctx, name)
	if e != nil || v.UID != uid {
		return nil, Err
	}
	key, e := c.guestKey(ctx, v.Image)
	if e != nil {
		return nil, e
	}
	dial := func(ctx context.Context) (net.Conn, error) {
		u, _ := url.Parse(c.Base + "/vms/" + url.PathEscape(name) + "/port-forward?port=22&wait=5")
		origin := u.Scheme + "://" + u.Host
		if u.Scheme == "https" {
			u.Scheme = "wss"
		} else {
			u.Scheme = "ws"
		}
		cfg, e := websocket.NewConfig(u.String(), origin)
		if e != nil {
			return nil, e
		}
		cfg.Header.Set("User-Agent", "pulse-tart")
		if tr, ok := c.HTTP.Transport.(*http.Transport); ok {
			cfg.TlsConfig = tr.TLSClientConfig
		}
		if c.token != "" {
			r, _ := http.NewRequest("GET", origin, nil)
			r.SetBasicAuth(c.user, c.token)
			cfg.Header.Set("Authorization", r.Header.Get("Authorization"))
		}
		ws, e := cfg.DialContext(ctx)
		if e != nil {
			return nil, e
		}
		ws.PayloadType = websocket.BinaryFrame
		return ws, nil
	}
	return guestconn.Exec(ctx, dial, v.Username, v.Password, key, script)
}
func (c *Client) Stop(ctx context.Context, name, uid string) error {
	return c.power(ctx, name, uid, "stopped")
}
func (c *Client) power(ctx context.Context, name, uid, power string) error {
	var v map[string]json.RawMessage
	path := "/vms/" + url.PathEscape(name)
	if e := c.call(ctx, "GET", path, nil, &v); e != nil {
		return e
	}
	var got string
	if json.Unmarshal(v["uid"], &got) != nil || got != uid {
		return Err
	}
	v["powerState"], _ = json.Marshal(power)
	return c.call(ctx, "PUT", path, v, nil)
}

// Endpoint requests the journal's fixed worker port. A listener is not health:
// the primary must authenticate and verify Docker before admission.
func (c *Client) Endpoint(ctx context.Context, name, uid string, port int) (bool, error) {
	var v map[string]json.RawMessage
	path := "/vms/" + url.PathEscape(name)
	if e := c.call(ctx, "GET", path, nil, &v); e != nil {
		return false, e
	}
	var got string
	if json.Unmarshal(v["uid"], &got) != nil || got != uid {
		return false, Err
	}
	var endpoints []map[string]any
	_ = json.Unmarshal(v["endpoints"], &endpoints)
	found := false
	for _, e := range endpoints {
		if e["name"] == "pulse-engine-ssh" {
			found = true
			target, _ := e["target"].(map[string]any)
			vm, _ := target["vm"].(map[string]any)
			if e["protocol"] != "tcp" || vm["port"] != float64(2222) {
				return false, Err
			}
			pr, ok := e["workerPortRange"].(map[string]any)
			if !ok || pr["min"] != float64(port) || pr["max"] != float64(port) {
				return false, Err
			}
		}
	}
	if !found {
		endpoints = append(endpoints, map[string]any{"name": "pulse-engine-ssh", "protocol": "tcp", "workerPortRange": map[string]int{"min": port, "max": port}, "target": map[string]any{"vm": map[string]int{"port": 2222}}})
		v["endpoints"], _ = json.Marshal(endpoints)
		return false, c.call(ctx, "PUT", path, v, nil)
	}
	var observed []struct {
		Name, State string
		WorkerPort  int
	}
	_ = json.Unmarshal(v["observedEndpoints"], &observed)
	for _, e := range observed {
		if e.Name == "pulse-engine-ssh" && e.State == "listening" && e.WorkerPort == port {
			return true, nil
		}
	}
	return false, nil
}

// Clones retain their source host key. Never silently drop a configured pin
// just because the immediate source is a local prepared image.
func (c *Client) guestKey(ctx context.Context, image string) ([]byte, error) {
	if key, ok := c.GuestKeys[image]; ok {
		return key, nil
	}
	pinned := false
	for _, key := range c.GuestKeys {
		if len(key) > 0 {
			pinned = true
		}
	}
	if !pinned {
		return nil, nil
	}
	sources, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	return inheritedGuestKey(c.GuestKeys, sources, image)
}
func inheritedGuestKey(keys map[string][]byte, sources []backend.VM, image string) ([]byte, error) {
	seen := map[string]bool{}
	for !seen[image] {
		seen[image] = true
		if key, ok := keys[image]; ok {
			return key, nil
		}
		parent := ""
		for _, v := range sources {
			if v.LocalName == image {
				parent = v.Image
				break
			}
		}
		if parent == "" {
			break
		}
		image = parent
	}
	return nil, Err
}
