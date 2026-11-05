// Diagnostic transport for the reproducible prototype suite. No provider CLI.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	docker "github.com/colony-2/pulse-tart/experimental/docker"
	"os"
	"time"
)

func main() {
	state := flag.String("state", "", "private connection JSON")
	command := flag.String("command", "/bin/bash -s", "guest command; payload on stdin")
	seconds := flag.Int("timeout", 120, "seconds")
	flag.Parse()
	b, e := os.ReadFile(*state)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	var j struct {
		URL                             string
		Spec                            struct{ Name, Username, Password string }
		Address                         string
		ControllerUser, ControllerToken string
	}
	if json.Unmarshal(b, &j) != nil {
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*seconds)*time.Second)
	defer cancel()
	g := docker.Guest{OrchardURL: j.URL, VM: j.Spec.Name, User: j.Spec.Username, Password: j.Spec.Password, Address: j.Address, ControllerUser: j.ControllerUser, ControllerToken: j.ControllerToken}
	if e = g.Exec(ctx, *command, os.Stdin, os.Stdout, os.Stderr); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
