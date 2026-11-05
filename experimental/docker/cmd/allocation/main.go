// Private harness adapter exercising the shared Provider API, not a provider plugin.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	api "github.com/colony-2/pulse-provider-api"
	docker "github.com/colony-2/pulse-tart/experimental/docker"
	"os"
	"time"
)

func main() {
	state := flag.String("state", "", "private VM JSON")
	journal := flag.String("journal", "", "private allocation journal")
	action := flag.String("action", "submit", "submit, list, capabilities")
	flag.Parse()
	raw, e := os.ReadFile(*state)
	if e != nil {
		die(e)
	}
	var j struct {
		URL, Address, UID string
		Spec              struct{ Name, Username, Password, Image string }
	}
	if e = json.Unmarshal(raw, &j); e != nil {
		die(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	p, e := docker.NewLinux(ctx, docker.Config{Enabled: true, NativeID: j.UID, StateDir: *journal, Base: "ubuntu-24.04-arm64", Image: j.Spec.Image, CPU: 4, MemoryMiB: 7680, Guest: docker.Guest{OrchardURL: j.URL, VM: j.Spec.Name, Address: j.Address, User: j.Spec.Username, Password: j.Spec.Password}})
	if e != nil {
		die(e)
	}
	defer p.Close()
	var out any
	switch *action {
	case "submit":
		var l api.Launch
		if e = json.NewDecoder(os.Stdin).Decode(&l); e != nil {
			die(fmt.Errorf("invalid assignment JSON"))
		}
		out, e = p.Submit(ctx, []api.Launch{l})
	case "list":
		out, e = p.List(ctx, api.ListRequest{})
	case "capabilities":
		out, e = p.GetCapabilities(ctx)
	default:
		die(fmt.Errorf("invalid action"))
	}
	if e != nil {
		die(e)
	}
	json.NewEncoder(os.Stdout).Encode(out)
}
func die(e error) { fmt.Fprintln(os.Stderr, e); os.Exit(1) }
