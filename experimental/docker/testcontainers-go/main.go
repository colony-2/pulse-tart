package main

import (
	"context"
	"fmt"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c, e := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: tc.ContainerRequest{Image: "python:3.13-alpine", ExposedPorts: []string{"8080/tcp"}, Cmd: []string{"python", "-m", "http.server", "8080"}, WaitingFor: wait.ForHTTP("/").WithPort("8080/tcp")}, Started: true})
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	defer c.Terminate(context.Background())
	host, e := c.Host(ctx)
	if e != nil {
		panic(e)
	}
	port, e := c.MappedPort(ctx, "8080/tcp")
	if e != nil {
		panic(e)
	}
	r, e := http.Get("http://" + host + ":" + port.Port())
	if e != nil {
		panic(e)
	}
	defer r.Body.Close()
	io.Copy(io.Discard, r.Body)
	if r.StatusCode != 200 {
		panic(r.Status)
	}
	fmt.Println("PULSE_GO_TESTCONTAINERS_OK")
}
