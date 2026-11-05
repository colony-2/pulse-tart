// Package docker contains an opt-in, cold-boot Docker allocation experiment.
// It is not enabled by the normal macOS provider or its default catalog.
package docker

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/net/websocket"
)

// Guest connects to a disposable VM. Orchard forwards SSH over its API; Address
// is used for direct Tart. Credentials never belong in shared workspaces.
type Guest struct {
	OrchardURL, VM, Address, User, Password, ControllerUser, ControllerToken string
}

// DialSSH opens only the VM's SSH port; used by the controller-side lab tunnel.
func (g Guest) DialSSH(ctx context.Context) (net.Conn, error) {
	var conn net.Conn
	var err error
	if g.OrchardURL != "" {
		u, err := url.Parse(strings.TrimSuffix(g.OrchardURL, "/") + "/vms/" + url.PathEscape(g.VM) + "/port-forward")
		if err != nil {
			return nil, fmt.Errorf("invalid controller URL")
		}
		origin := u.Scheme + "://" + u.Host
		if u.Scheme == "https" {
			u.Scheme = "wss"
		} else if u.Scheme == "http" {
			u.Scheme = "ws"
		} else {
			return nil, fmt.Errorf("invalid controller scheme")
		}
		u.RawQuery = url.Values{"port": {"22"}, "wait": {"5"}}.Encode()
		cfg, err := websocket.NewConfig(u.String(), origin)
		if err != nil {
			return nil, err
		}
		cfg.Header.Set("User-Agent", "pulse-tart-docker-prototype")
		cfg.Dialer = &net.Dialer{Timeout: 10 * time.Second}
		if g.ControllerToken != "" {
			r, _ := http.NewRequest("GET", origin, nil)
			r.SetBasicAuth(g.ControllerUser, g.ControllerToken)
			cfg.Header.Set("Authorization", r.Header.Get("Authorization"))
		}
		ws, e := websocket.DialConfig(cfg)
		if e != nil {
			return nil, fmt.Errorf("guest forwarding unavailable: %w", e)
		}
		ws.PayloadType = websocket.BinaryFrame
		conn = ws
	} else {
		conn, err = (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", g.Address)
		if err != nil {
			return nil, fmt.Errorf("guest connection unavailable")
		}
	}
	return conn, nil
}

func (g Guest) Connect(ctx context.Context) (*ssh.Client, error) {
	conn, err := g.DialSSH(ctx)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	deadline := time.Now().Add(15 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetDeadline(deadline)
	cc, ch, req, err := ssh.NewClientConn(conn, g.VM, &ssh.ClientConfig{User: g.User, Auth: []ssh.AuthMethod{ssh.Password(g.Password)}, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	stop()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("guest SSH unavailable: %w", err)
	}
	conn.SetDeadline(time.Time{})
	return ssh.NewClient(cc, ch, req), nil
}

// Exec uses stdin for scripts, binaries and leases, and never prints credentials.
func (g Guest) Exec(ctx context.Context, command string, in io.Reader, out, stderr io.Writer) error {
	var c *ssh.Client
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		c, err = g.Connect(ctx)
		if err == nil {
			break
		}
		if attempt == 2 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	// Only connection establishment is retried. Once Exec is dispatched, a
	// failure is ambiguous and must never cause replay of a lease handoff.
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	s, err := c.NewSession()
	if err != nil {
		return fmt.Errorf("guest session unavailable")
	}
	defer s.Close()
	s.Stdin = in
	s.Stdout = out
	s.Stderr = stderr
	if err = s.Run(command); err != nil {
		return fmt.Errorf("guest command failed: %w", err)
	}
	return nil
}
