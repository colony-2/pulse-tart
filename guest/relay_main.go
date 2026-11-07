package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func serveDockerRelay(path string) error {
	var c struct{ Address, User, KeyFile, HostKeyFile, Socket string }
	b, e := os.ReadFile(path)
	if e == nil {
		e = json.Unmarshal(b, &c)
	}
	if e != nil {
		return fmt.Errorf("invalid private relay configuration")
	}
	key, e := os.ReadFile(c.KeyFile)
	if e != nil {
		return fmt.Errorf("missing allocation key")
	}
	host, e := os.ReadFile(c.HostKeyFile)
	if e != nil {
		return fmt.Errorf("missing engine host key")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	dial, closeAll, e := SSHTransport(ctx, c.Address, c.User, key, host)
	if e != nil {
		return e
	}
	defer closeAll()
	r := NewRelay(ctx, dial)
	defer r.Close()
	if e = r.Serve(c.Socket); e != nil {
		return e
	}
	return nil
}
