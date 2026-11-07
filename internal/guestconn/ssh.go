// Package guestconn supplies bounded, cancellation-aware management SSH.
package guestconn

import (
	"bytes"
	"context"
	"errors"
	"golang.org/x/crypto/ssh"
	"io"
	"net"
	"time"
)

var Err = errors.New("guest operation unavailable")

type output struct{ bytes.Buffer }

func (b *output) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 1<<20 {
		return 0, Err
	}
	return b.Buffer.Write(p)
}
func Exec(ctx context.Context, dial func(context.Context) (net.Conn, error), user, password string, key, script []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, e := dial(ctx)
	if e != nil {
		return nil, Err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	callback := ssh.InsecureIgnoreHostKey()
	if len(key) > 0 {
		pub, _, _, _, e := ssh.ParseAuthorizedKey(key)
		if e != nil {
			return nil, Err
		}
		callback = ssh.FixedHostKey(pub)
	}
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	cc, ch, req, e := ssh.NewClientConn(conn, "guest", &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.Password(password)}, HostKeyCallback: callback})
	if e != nil {
		return nil, Err
	}
	conn.SetDeadline(time.Time{})
	client := ssh.NewClient(cc, ch, req)
	defer client.Close()
	session, e := client.NewSession()
	if e != nil {
		return nil, Err
	}
	defer session.Close()
	var out output
	session.Stdin = bytes.NewReader(script)
	session.Stdout = &out
	session.Stderr = io.Discard
	if e = session.Run("/bin/bash -s"); e != nil {
		return nil, Err
	}
	return out.Bytes(), nil
}
