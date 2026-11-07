package tart

import (
	"context"
	"github.com/colony-2/pulse-tart/internal/guestconn"
	"net"
	"strings"
)

func (b *Client) Exec(ctx context.Context, name, uid string, script []byte) ([]byte, error) {
	if e := b.mu.LockContext(ctx); e != nil {
		return nil, e
	}
	v, e := b.get(ctx, name)
	if e != nil || v.UID != uid || b.records[name] == nil {
		b.mu.Unlock()
		return nil, Err
	}
	key := append([]byte{}, b.records[name].HostKey...)
	b.mu.Unlock()
	data, e := b.command(ctx, "ip", name, "--wait", "5")
	if e != nil {
		return nil, e
	}
	ip := strings.TrimSpace(string(data))
	if net.ParseIP(ip) == nil {
		return nil, Err
	}
	return guestconn.Exec(ctx, func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{Timeout: b.cfg.SSHTimeout}).DialContext(ctx, "tcp", net.JoinHostPort(ip, "22"))
	}, v.Username, v.Password, key, script)
}
func (b *Client) Stop(ctx context.Context, name, uid string) error {
	if e := b.mu.LockContext(ctx); e != nil {
		return e
	}
	defer b.mu.Unlock()
	v, e := b.get(ctx, name)
	if e != nil || v.UID != uid {
		return Err
	}
	if v.Stopped() {
		return nil
	}
	_, e = b.command(ctx, "stop", name)
	return e
}
