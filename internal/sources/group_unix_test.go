//go:build unix

package sources

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunStopsChildren(t *testing.T) {
	// A "yt-dlp" that starts a child, like Bun, which keeps its output open.
	dir := t.TempDir()
	script, pidfile := filepath.Join(dir, "yt-dlp"), filepath.Join(dir, "child.pid")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30 &\necho $! > \"$1\"\nwait\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		run(ctx, script, pidfile)
		close(done)
	}()
	var pid int
	for deadline := time.Now().Add(5 * time.Second); pid == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the child never started")
		}
		b, _ := os.ReadFile(pidfile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}

	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		syscall.Kill(pid, syscall.SIGKILL)
		t.Fatal("canceling waited for the child")
	}
	// The child was stopped with it, not left running.
	for deadline := time.Now().Add(2 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("the child kept running")
		}
	}
}

func TestRunStopsChildrenOfACrash(t *testing.T) {
	dir := t.TempDir()
	script, pidfile := filepath.Join(dir, "extractor"), filepath.Join(dir, "child.pid")
	// A crashing parent, rather than a still-running parent being cancelled.
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 5 &\necho $! > \"$1\"\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := run(ctx, script, pidfile)
	if err == nil {
		t.Fatal("parent unexpectedly succeeded")
	}
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	// kill(pid, 0) sees zombies too, so the state says whether it's gone.
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		_, state, _ := strings.Cut(string(stat), ") ")
		if err != nil || strings.HasPrefix(state, "Z") || strings.HasPrefix(state, "X") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("yt-dlp crashed, and its child kept running (state %.1s)", state)
		}
	}
}
