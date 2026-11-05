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

func read(path string) docker.Guest {
	var j struct {
		URL, Address string
		Spec         struct{ Name, Username, Password string }
	}
	b, e := os.ReadFile(path)
	if e != nil {
		panic("private VM connection missing")
	}
	if json.Unmarshal(b, &j) != nil {
		panic("invalid private VM connection")
	}
	return docker.Guest{OrchardURL: j.URL, VM: j.Spec.Name, Address: j.Address, User: j.Spec.Username, Password: j.Spec.Password}
}
func main() {
	primary := flag.String("primary", "", "private primary VM JSON")
	engine := flag.String("engine", "", "private engine VM JSON")
	port := flag.Int("port", 22443, "primary loopback port")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if e := docker.BridgeEngine(ctx, read(*primary), read(*engine), func() { fmt.Println("PULSE_ENGINE_TUNNEL_READY") }, *port); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
