package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLiteralProcessAndEOF(t *testing.T) {
	if os.Getenv("PULSE_HELPER") == "yes" {
		var input struct {
			Args  []string
			Env   string
			Cwd   string
			Stdin string
		}
		input.Args = os.Args[3:]
		input.Env = os.Getenv("literal")
		input.Cwd, _ = os.Getwd()
		b, _ := os.ReadFile("/dev/stdin")
		input.Stdin = string(b)
		_ = json.NewEncoder(os.Stdout).Encode(input)
		os.Exit(0)
	}
	for _, stdin := range []string{"", "one\n☃\n"} {
		dir := t.TempDir()
		exe, _ := os.Executable()
		e := Envelope{Version: 1, LaunchID: "literal", Process: Process{Command: []string{exe, "-test.run=^TestLiteralProcessAndEOF$", "--"}, Args: []string{"$HOME", "`x`", "quotes'\"", "a\nb"}, Env: map[string]string{"PULSE_HELPER": "yes", "literal": "$HOME `not executed`"}, WorkingDir: dir, Stdin: stdin}}
		code, err := execute(e, dir, uint32(os.Getuid()), uint32(os.Getgid()))
		if err != nil || code != 0 {
			t.Fatal(code, err)
		}
		b, _ := os.ReadFile(filepath.Join(dir, "worker.log"))
		var got struct {
			Args            []string
			Env, Cwd, Stdin string
		}
		if json.Unmarshal(b, &got) != nil || got.Env != e.Process.Env["literal"] || got.Stdin != stdin || got.Cwd != canonical(dir) || strings.Join(got.Args, "|") != strings.Join(e.Process.Args, "|") {
			t.Fatalf("literal contract changed: %s", b)
		}
	}
}
func TestIngestOneShot(t *testing.T) {
	dir := t.TempDir()
	e := Envelope{Version: 1, LaunchID: "a", Process: Process{Command: []string{"c2j"}, Stdin: "lease"}}
	b, _ := json.Marshal(e)
	if err := ingest(dir, strings.NewReader(string(b))); err != nil {
		t.Fatal(err)
	}
	if err := ingest(dir, strings.NewReader(string(b))); err != nil {
		t.Fatal(err)
	}
	e.Process.Stdin = "other"
	b, _ = json.Marshal(e)
	if err := ingest(dir, strings.NewReader(string(b))); err == nil {
		t.Fatal("conflicting envelope accepted")
	}
	for _, bad := range []string{`{}`, `{"version":1} junk`, strings.Repeat(" ", 17<<20)} {
		if _, err := decode(strings.NewReader(bad)); err == nil {
			t.Fatal("invalid envelope")
		}
	}
}
func TestResolveUsesWorkerPath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "testcmd")
	if e := os.WriteFile(p, []byte("#!/bin/sh\n"), 0700); e != nil {
		t.Fatal(e)
	}
	got, e := resolve("testcmd", dir, []string{"PATH=" + dir})
	if e != nil || got != p {
		t.Fatal(got, e)
	}
}

func TestDeliveryBoundaryAndFailedStartIntent(t *testing.T) {
	e := Envelope{Version: 1, LaunchID: "boundary", Process: Process{Command: []string{"/nonexistent-worker"}, Stdin: strings.Repeat("x", 1<<20)}}
	b, _ := json.Marshal(e)
	if _, err := decode(strings.NewReader(string(b))); err != nil {
		t.Fatal(err)
	}
	e.Process.Stdin += "x"
	b, _ = json.Marshal(e)
	if _, err := decode(strings.NewReader(string(b))); err == nil {
		t.Fatal("oversized stdin accepted")
	}
	e.Process.Stdin = ""
	dir := t.TempDir()
	if _, err := execute(e, dir, uint32(os.Getuid()), uint32(os.Getgid())); err == nil {
		t.Fatal("missing command succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir, "intent")); err != nil {
		t.Fatal("failed start lost one-shot intent", err)
	}
}

func canonical(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return resolved
}

func TestOpaqueLeaseBytesSurviveJournalAndDelivery(t *testing.T) {
	if os.Getenv("PULSE_BYTES_HELPER") == "1" {
		_, _ = io.Copy(os.Stdout, os.Stdin)
		os.Exit(0)
	}
	dir := t.TempDir()
	binary, _ := os.Executable()
	lease := []byte{'x', 0, 0xff, 0xfe, '\n'}
	e := Envelope{Version: 1, LaunchID: "binary-lease", Process: Process{Command: []string{binary, "-test.run=^TestOpaqueLeaseBytesSurviveJournalAndDelivery$"}, Env: map[string]string{"PULSE_BYTES_HELPER": "1"}, WorkingDir: dir, StdinBytes: lease}}
	data, _ := json.Marshal(e)
	if err := ingest(dir, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	stored, err := os.Open(filepath.Join(dir, "launch.json"))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decode(stored)
	stored.Close()
	if err != nil {
		t.Fatal(err)
	}
	code, err := execute(decoded, dir, uint32(os.Getuid()), uint32(os.Getgid()))
	if err != nil || code != 0 {
		t.Fatal("worker failed", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "worker.log"))
	if err != nil || !bytes.Equal(got, lease) {
		t.Fatal("opaque lease bytes changed")
	}
}
