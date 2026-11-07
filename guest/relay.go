package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

// Relay is a macOS-local Docker Unix socket and TCP publication
// gateway. Its only upstream is a private allocation's SSH-authenticated daemon.
// UDP and arbitrary container-to-job callbacks are deliberately unsupported.
type Relay struct {
	mu        sync.Mutex
	transport *http.Transport
	client    *http.Client
	dial      func(context.Context, string, string) (net.Conn, error)
	ports     map[string]net.Listener
	ctx       context.Context
	cancel    context.CancelFunc
}

// NewRelay accepts the authenticated allocation transport. Unix socket dials
// target Linux's /var/run/docker.sock; TCP dials target its loopback services.
func NewRelay(parent context.Context, dial func(context.Context, string, string) (net.Conn, error)) *Relay {
	ctx, cancel := context.WithCancel(parent)
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dial(ctx, "unix", "/var/run/docker.sock")
	}}
	return &Relay{transport: tr, client: &http.Client{Transport: tr, Timeout: 30 * time.Second}, dial: dial, ports: map[string]net.Listener{}, ctx: ctx, cancel: cancel}
}
func (r *Relay) Close() {
	r.cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.ports {
		l.Close()
	}
	r.ports = map[string]net.Listener{}
	r.transport.CloseIdleConnections()
}
func (r *Relay) request(ctx context.Context, method, path string) (*http.Response, error) {
	q, e := http.NewRequestWithContext(ctx, method, "http://engine"+path, nil)
	if e != nil {
		return nil, e
	}
	return r.client.Do(q)
}

// Reconcile also runs at relay startup, discovering already-running containers.
func (r *Relay) Reconcile(ctx context.Context) error { return r.reconcile(ctx, "") }
func (r *Relay) reconcile(ctx context.Context, target string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	res, e := r.request(ctx, "GET", "/containers/json")
	if e != nil {
		return fmt.Errorf("engine inventory unavailable: %w", e)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("engine inventory HTTP %d", res.StatusCode)
	}
	var containers []struct {
		Ports []struct {
			PublicPort int
			Type       string
		}
	}
	if e = json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&containers); e != nil {
		return e
	}
	wanted := map[string]bool{}
	for _, c := range containers {
		for _, p := range c.Ports {
			if p.Type == "tcp" && p.PublicPort > 0 {
				wanted[strconv.Itoa(p.PublicPort)] = true
			}
		}
	}
	for port, l := range r.ports {
		if !wanted[port] {
			l.Close()
			delete(r.ports, port)
		}
	}
	var firstError error
	fresh := map[string]net.Listener{}
	for port := range wanted {
		if r.ports[port] != nil {
			continue
		}
		l, e := net.Listen("tcp4", "127.0.0.1:"+port)
		if e != nil {
			affected := target == ""
			if !affected {
				inspected, err := r.request(ctx, "GET", target+"/json")
				if err != nil {
					affected = true
				} else {
					var c struct {
						NetworkSettings struct {
							Ports map[string][]struct{ HostPort string }
						}
					}
					decodeErr := json.NewDecoder(io.LimitReader(inspected.Body, 1<<20)).Decode(&c)
					inspected.Body.Close()
					if inspected.StatusCode != 200 || decodeErr != nil {
						affected = true
					} else {
						for _, bindings := range c.NetworkSettings.Ports {
							for _, binding := range bindings {
								if binding.HostPort == port {
									affected = true
								}
							}
						}
					}
				}
			}
			if affected && firstError == nil {
				firstError = fmt.Errorf("macOS publication port %s unavailable", port)
			}
			continue
		}
		fresh[port] = l
	}
	for port, l := range fresh {
		r.ports[port] = l
		go r.forward(l, port)
	}
	return firstError
}
func (r *Relay) forward(l net.Listener, port string) {
	for {
		local, e := l.Accept()
		if e != nil {
			return
		}
		go func() {
			defer local.Close()
			remote, e := r.dial(r.ctx, "tcp", "127.0.0.1:"+port)
			if e != nil {
				return
			}
			defer remote.Close()
			stop := context.AfterFunc(r.ctx, func() { local.Close(); remote.Close() })
			defer stop()
			done := make(chan struct{})
			go func() {
				io.Copy(remote, local)
				if c, ok := remote.(interface{ CloseWrite() error }); ok {
					c.CloseWrite()
				}
				close(done)
			}()
			io.Copy(local, remote)
			local.Close()
			remote.Close()
			<-done
		}()
	}
}
func (r *Relay) Handler() http.Handler {
	target, _ := url.Parse("http://engine")
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = r.transport
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, q *http.Request, e error) {
		http.Error(w, "Docker relay unavailable", http.StatusBadGateway)
	}
	proxy.ModifyResponse = func(res *http.Response) error {
		q := res.Request
		if q.Method == "POST" && res.StatusCode >= 200 && res.StatusCode < 300 && strings.Contains(q.URL.Path, "/containers/") && (strings.HasSuffix(q.URL.Path, "/start") || strings.HasSuffix(q.URL.Path, "/restart")) {
			target := q.URL.Path[:strings.LastIndex(q.URL.Path, "/")]
			if e := r.reconcile(q.Context(), target); e != nil {
				// A failed local bind must not look like a successful service start.
				stop := q.URL.Path[:strings.LastIndex(q.URL.Path, "/")] + "/stop?t=0"
				stopped, err := r.request(q.Context(), "POST", stop)
				if err == nil {
					io.Copy(io.Discard, stopped.Body)
					stopped.Body.Close()
				}
				res.Body.Close()
				return e
			}
		}
		return nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		// Build uploads and streaming responses may overlap on HTTP/1.x.
		_ = http.NewResponseController(w).EnableFullDuplex()
		proxy.ServeHTTP(w, q)
	})
}
func (r *Relay) Serve(socket string) error {
	// Serialize restarts, and remove only this user's refused stale Unix socket.
	lock, e := os.OpenFile(socket+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		return fmt.Errorf("relay already running")
	}
	if st, e := os.Lstat(socket); e == nil {
		owner, ok := st.Sys().(*syscall.Stat_t)
		if st.Mode()&os.ModeSocket == 0 || !ok || owner.Uid != uint32(os.Geteuid()) {
			return fmt.Errorf("refusing unowned socket path")
		}
		c, err := net.DialTimeout("unix", socket, time.Second)
		if err == nil {
			c.Close()
			return fmt.Errorf("socket already active")
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			return fmt.Errorf("socket state uncertain")
		}
		if e = os.Remove(socket); e != nil {
			return e
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	l, e := net.Listen("unix", socket)
	if e != nil {
		return e
	}
	defer l.Close()
	defer os.Remove(socket)
	if e = os.Chmod(socket, 0660); e != nil {
		return e
	}
	if e = r.Reconcile(r.ctx); e != nil {
		return e
	}
	server := &http.Server{Handler: r.Handler(), ReadHeaderTimeout: 15 * time.Second}
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.ctx.Done():
				server.Close()
				return
			case <-ticker.C:
				_ = r.Reconcile(r.ctx)
			}
		}
	}()
	e = server.Serve(l)
	if e == http.ErrServerClosed {
		return nil
	}
	return e
}

// SSHTransport requires a per-allocation private key and pinned host key. It
// reconnects only while opening a channel, before Docker request bytes are sent.
func SSHTransport(ctx context.Context, address, user string, key, hostKey []byte) (func(context.Context, string, string) (net.Conn, error), func(), error) {
	signer, e := ssh.ParsePrivateKey(key)
	if e != nil {
		return nil, nil, fmt.Errorf("invalid allocation key")
	}
	pinned, _, _, _, e := ssh.ParseAuthorizedKey(hostKey)
	if e != nil {
		return nil, nil, fmt.Errorf("invalid engine host key")
	}
	var mu sync.Mutex
	var client *ssh.Client
	closed := false
	closeAll := func() {
		mu.Lock()
		defer mu.Unlock()
		closed = true
		if client != nil {
			client.Close()
		}
	}
	dial := func(call context.Context, network, target string) (net.Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		if closed || ctx.Err() != nil {
			return nil, fmt.Errorf("closed transport")
		}
		for attempt := 0; attempt < 2; attempt++ {
			if client == nil {
				conn, e := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(call, "tcp", address)
				if e != nil {
					return nil, e
				}
				stop := context.AfterFunc(call, func() { conn.Close() })
				conn.SetDeadline(time.Now().Add(10 * time.Second))
				cc, ch, req, e := ssh.NewClientConn(conn, address, &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.FixedHostKey(pinned), HostKeyAlgorithms: []string{pinned.Type()}})
				stop()
				if e != nil {
					conn.Close()
					return nil, e
				}
				conn.SetDeadline(time.Time{})
				client = ssh.NewClient(cc, ch, req)
			}
			c, e := client.DialContext(call, network, target)
			if e == nil {
				return c, nil
			}
			client.Close()
			client = nil
		}
		return nil, fmt.Errorf("allocation SSH channel unavailable")
	}
	return dial, closeAll, nil
}
