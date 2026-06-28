package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hermes/conclave-mcp/cmd/rooms"
)

func TestRootCmd_Version(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := newRootCmd()
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"version"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "conclave-mcp 0.1.0-dev") {
		t.Errorf("stdout: got %q, want to contain version string", out)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr should be empty, got %q", stderr.String())
	}
}

func TestRootCmd_VersionFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := newRootCmd()
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"--version"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(stdout.String(), "conclave-mcp 0.1.0-dev") {
		t.Errorf("stdout: got %q", stdout.String())
	}
}

func TestRootCmd_Help(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := newRootCmd()
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"--help"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "Conclave MCP") {
		t.Errorf("help should mention app name, got %q", out)
	}
	if !strings.Contains(out, "serve") {
		t.Errorf("help should list serve subcommand, got %q", out)
	}
	if !strings.Contains(out, "version") {
		t.Errorf("help should list version subcommand, got %q", out)
	}
}

func TestRootCmd_NoArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := newRootCmd()
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{})

	// No args -> cobra prints help. We just want to confirm no
	// panic and a non-error exit.
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if stdout.Len() == 0 {
		t.Errorf("expected help on stdout, got empty")
	}
}

func TestRootCmd_UnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := newRootCmd()
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"frobnicate"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for unknown subcommand, got nil")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("error should mention 'unknown command', got %q", err.Error())
	}
}

func TestRootCmd_ServeFlagOverride(t *testing.T) {
	// root.Execute() blocks until the serve subcommand returns.
	// serve.Execute blocks on srv.Start(ctx) until ctx is cancelled.
	// We give cobra a cancellable context, call cancel() before
	// returning, and assert on the captured stderr. The server logs
	// "config loaded" and "server starting" before Start, so those
	// lines are guaranteed to be in stderr by the time cancel fires.
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	root := newRootCmd()
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetContext(ctx)
	root.SetArgs([]string{"serve", "--port", "9999"})

	// Run Execute in a goroutine so we can cancel after the boot
	// log lines have flushed. root.Execute() returns once the
	// server's Shutdown has been driven by the cancel.
	done := make(chan error, 1)
	go func() { done <- root.Execute() }()

	// Give the boot log a moment to flush, then signal shutdown.
	// 100ms is generous on a fast box and far below the 30s
	// 10s-graceful-shutdown budget in server.Start.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("root.Execute did not return within 15s after cancel; server.Start likely ignores ctx")
	}

	if !strings.Contains(stderr.String(), `"port":9999`) {
		t.Errorf("expected port 9999 in logs, got %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"bind":"127.0.0.1:9999"`) {
		t.Errorf("expected bind 127.0.0.1:9999 in logs, got %s", stderr.String())
	}
}

func TestRootCmd_RoomsExitCodePropagates(t *testing.T) {
	// A rooms subcommand with no --token and no env token must exit
	// with code 2 (caller misuse) per SPEC-0004 §4, not 0 and not
	// the cobra default of 2 (which happens to match but is the
	// wrong reason; we want to prove rooms.ExitError is read).
	t.Setenv("CONCLAVE_API_KEY", "")
	var stdout, stderr bytes.Buffer
	root := newRootCmd()
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"rooms", "list"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error from rooms list without token")
	}
	// Direct check: rooms.ExitCode(err) returns 2 because the
	// *exitError wraps a client-construction failure.
	if got := rooms.ExitCode(err); got != 2 {
		t.Errorf("rooms.ExitCode(err)=%d, want 2 (err=%v)", got, err)
	}
}
