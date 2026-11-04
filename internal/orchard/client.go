package orchard

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/colony-2/pulse-tart/internal/backend"
	"github.com/colony-2/pulse-tart/internal/config"
)

const MaxBody = 32 << 20

var Err = errors.New("Orchard request failed")

type Error = backend.Error

var IsStatus = backend.IsStatus

type Script = backend.Script
type Spec = backend.Spec
type Condition = backend.Condition
type VM = backend.VM
type Event = backend.Event
type Client struct {
	Base        string
	HTTP        *http.Client
	user, token string
}

func New(c config.Orchard) (*Client, error) {
	base, e := config.NormalizeURL(c.URL, c.AllowInsecureHTTP)
	if e != nil {
		return nil, e
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// A controller connection must not be redirected through an inherited proxy.
	tr.Proxy = nil
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if len(c.CACert) > 0 {
		b := c.CACert
		pool, e := x509.SystemCertPool()
		if e != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(b) {
			return nil, config.Err
		}
		tr.TLSClientConfig.RootCAs = pool
	}
	token := string(c.Token)
	if (token == "") != (c.Username == "") {
		return nil, config.Err
	}
	timeout := c.RequestTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	if timeout < 0 {
		return nil, config.Err
	}
	return &Client{Base: base, HTTP: &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, user: c.Username, token: token}, nil
}
func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, e := json.Marshal(in)
		if e != nil {
			return Err
		}
		body = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if e != nil {
		return Err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.SetBasicAuth(c.user, c.token)
	}
	resp, e := c.HTTP.Do(req)
	if e != nil {
		return Err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &Error{Code: resp.StatusCode}
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if e != nil || len(b) > MaxBody {
		return Err
	}
	if out != nil && json.Unmarshal(b, out) != nil {
		return Err
	}
	return nil
}
func (c *Client) Info(ctx context.Context) error {
	var info struct {
		Version string `json:"version"`
	}
	if e := c.call(ctx, "GET", "/controller/info", nil, &info); e != nil {
		return e
	}
	if info.Version == "" {
		return Err
	}
	return nil
}
func (c *Client) List(ctx context.Context) ([]VM, error) {
	var v []VM
	e := c.call(ctx, "GET", "/vms", nil, &v)
	if e == nil {
		if v == nil {
			return nil, Err
		}
		seen := map[string]bool{}
		for _, vm := range v {
			if vm.Name == "" || vm.UID == "" || seen[vm.Name] {
				return nil, Err
			}
			seen[vm.Name] = true
		}
	}
	return v, e
}
func (c *Client) Get(ctx context.Context, name string) (VM, error) {
	var v VM
	e := c.call(ctx, "GET", "/vms/"+url.PathEscape(name), nil, &v)
	return v, e
}
func (c *Client) Create(ctx context.Context, s Spec) (VM, error) {
	var v VM
	in := struct {
		Spec
		ImagePullPolicy string `json:"imagePullPolicy,omitempty"`
	}{Spec: s}
	if s.Suspendable {
		in.ImagePullPolicy = "IfNotPresent"
	}
	e := c.call(ctx, "POST", "/vms", in, &v)
	return v, e
}

// Preserve the complete API object: a partial PUT can clear fields we don't model.
// Suspend is idempotent and only used after ownership was verified by the caller.
func (c *Client) Suspend(ctx context.Context, name, uid string) error {
	var v map[string]json.RawMessage
	path := "/vms/" + url.PathEscape(name)
	if e := c.call(ctx, "GET", path, nil, &v); e != nil {
		return e
	}
	var gotUID, power string
	if json.Unmarshal(v["uid"], &gotUID) != nil || gotUID != uid {
		return Err
	}
	_ = json.Unmarshal(v["powerState"], &power)
	if power == "suspended" {
		return nil
	}
	if power != "running" {
		return Err
	}
	v["powerState"] = json.RawMessage(`"suspended"`)
	return c.call(ctx, "PUT", path, v, nil)
}
func (c *Client) Delete(ctx context.Context, name string) error {
	return c.call(ctx, "DELETE", "/vms/"+url.PathEscape(name), nil, nil)
}

func (c *Client) Events(ctx context.Context, name string) ([]Event, error) {
	var events []Event
	err := c.call(ctx, "GET", "/vms/"+url.PathEscape(name)+"/events", nil, &events)
	return events, err
}

func (c *Client) Close() error { c.HTTP.CloseIdleConnections(); return nil }
