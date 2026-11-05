package docker

import (
	"context"
	"fmt"
	"io"
	"net"
)

// BridgeEngine is a lab data path through the external controller. The macOS
// guest connects to its own loopback port; its authenticated engine SSH stream
// passes unchanged through this tunnel. No controller credential enters a guest.
// Production should provide an allocation-scoped worker-local equivalent.
func BridgeEngine(ctx context.Context, primary, engine Guest, ready func(), port int) error {
	c, e := primary.Connect(ctx)
	if e != nil {
		return e
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	listener, e := c.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if e != nil {
		return e
	}
	defer listener.Close()
	ready()
	for {
		in, e := listener.Accept()
		if e != nil {
			if ctx.Err() != nil {
				return nil
			}
			return e
		}
		go func(in net.Conn) {
			defer in.Close()
			out, e := engine.DialSSH(ctx)
			if e != nil {
				return
			}
			defer out.Close()
			stop := context.AfterFunc(ctx, func() { in.Close(); out.Close() })
			defer stop()
			done := make(chan struct{}, 2)
			go func() { io.Copy(out, in); done <- struct{}{} }()
			go func() { io.Copy(in, out); done <- struct{}{} }()
			<-done
			in.Close()
			out.Close()
			<-done
		}(in)
	}
}
