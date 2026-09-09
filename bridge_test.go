package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"tailscale.com/tsnet"
)

func TestStartBridge_CreatesSocketWithModeAndRemovesStale(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")

	// Leave a genuinely stale socket file on disk (nothing listening on
	// it any more), the way an unclean previous shutdown would: bind,
	// then close without the usual unlink-on-close.
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	stale.Close()
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("test setup: stale socket file not present: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := ResolvedBridge{
		BridgeConfig: BridgeConfig{Name: "test", Listen: sockPath, Target: "example.invalid:1"},
		Source:       "inline",
	}

	var wg sync.WaitGroup
	fatal := make(chan error, 1)
	srv := &tsnet.Server{} // never Up(); fine as long as no connection is dialed
	l, err := startBridge(ctx, srv, b, 0640, "", &wg, fatal)
	if err != nil {
		t.Fatalf("startBridge: %v", err)
	}

	info, err := os.Stat(sockPath)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if info.Mode().Perm() != 0640 {
		t.Errorf("want mode 0640, got %v", info.Mode().Perm())
	}

	cancel()
	if err := l.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	wg.Wait()

	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("want socket removed after Close, stat err = %v", err)
	}
}

func TestStartBridge_RefusesToRemoveNonSocketFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-a-socket")
	if err := os.WriteFile(path, []byte("do not delete me"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := ResolvedBridge{
		BridgeConfig: BridgeConfig{Name: "test", Listen: path, Target: "example.invalid:1"},
		Source:       "inline",
	}
	var wg sync.WaitGroup
	fatal := make(chan error, 1)
	srv := &tsnet.Server{}
	_, err := startBridge(ctx, srv, b, 0660, "", &wg, fatal)
	if err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("want 'not a socket' error, got: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file was removed: %v", err)
	}
	if string(data) != "do not delete me" {
		t.Fatalf("file contents changed: %q", data)
	}
}

func TestStartBridge_RefusesToStealLiveSocket(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "live.sock")

	live, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := ResolvedBridge{
		BridgeConfig: BridgeConfig{Name: "test", Listen: sockPath, Target: "example.invalid:1"},
		Source:       "inline",
	}
	var wg sync.WaitGroup
	fatal := make(chan error, 1)
	srv := &tsnet.Server{}
	_, err = startBridge(ctx, srv, b, 0660, "", &wg, fatal)
	if err == nil || !strings.Contains(err.Error(), "in use by another instance") {
		t.Fatalf("want 'in use by another instance' error, got: %v", err)
	}
}

func TestStartBridge_ChownsToGroup(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("chown to an arbitrary group requires root in this sandbox")
	}

	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := ResolvedBridge{
		BridgeConfig: BridgeConfig{Name: "test", Listen: sockPath, Target: "example.invalid:1"},
		Source:       "inline",
	}

	var wg sync.WaitGroup
	fatal := make(chan error, 1)
	srv := &tsnet.Server{}
	l, err := startBridge(ctx, srv, b, 0660, "root", &wg, fatal)
	if err != nil {
		t.Fatalf("startBridge: %v", err)
	}
	defer func() {
		cancel()
		l.Close()
		wg.Wait()
	}()

	info, err := os.Stat(sockPath)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	wantGID, err := lookupGID("root")
	if err != nil {
		t.Fatalf("lookupGID: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("expected *syscall.Stat_t from os.Stat on Linux")
	}
	if int(stat.Gid) != wantGID {
		t.Errorf("want socket group %d, got %d", wantGID, stat.Gid)
	}
}
