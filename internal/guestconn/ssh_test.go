package guestconn

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestCancellationClosesBlockedSSHHandshake(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	local, remote := net.Pipe()
	defer remote.Close()
	dialed := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := Exec(ctx, func(context.Context) (net.Conn, error) { close(dialed); return local, nil }, "user", "password", nil, []byte("private lease"))
		done <- err
	}()
	<-dialed
	cancel()
	select {
	case err := <-done:
		if err != Err {
			t.Fatalf("unredacted or missing error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SSH handshake ignored cancellation")
	}
}
