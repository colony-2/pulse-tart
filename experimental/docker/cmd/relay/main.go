package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	docker "github.com/colony-2/pulse-tart/experimental/docker"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	path := flag.String("config", "", "private allocation relay JSON")
	flag.Parse()
	var c struct{ Address, User, KeyFile, HostKeyFile, Socket string }
	b, e := os.ReadFile(*path)
	if e == nil {
		e = json.Unmarshal(b, &c)
	}
	if e != nil {
		panic("invalid private relay configuration")
	}
	key, e := os.ReadFile(c.KeyFile)
	if e != nil {
		panic("missing allocation key")
	}
	host, e := os.ReadFile(c.HostKeyFile)
	if e != nil {
		panic("missing engine host key")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	dial, closeAll, e := docker.SSHTransport(ctx, c.Address, c.User, key, host)
	if e != nil {
		panic(e)
	}
	defer closeAll()
	r := docker.NewRelay(ctx, dial)
	defer r.Close()
	if e = r.Serve(c.Socket); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
