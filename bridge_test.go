package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"tailscale.com/tsnet"
)

func TestStartBridge_CreatesSocketWithModeAndRemovesStale(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")

	// A stale file (e.g. left behind by an unclean previous shutdown)
	// should be removed rather than causing "address already in use".
	if err := os.WriteFile(sockPath, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := ResolvedBridge{
		BridgeConfig: BridgeConfig{Name: "test", Listen: sockPath, Target: "example.invalid:1"},
		Source:       "inline",
	}

	var wg sync.WaitGroup
	srv := &tsnet.Server{} // never Up(); fine as long as no connection is dialed
	l, err := startBridge(ctx, srv, b, 0640, "", &wg)
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
	srv := &tsnet.Server{}
	l, err := startBridge(ctx, srv, b, 0660, "root", &wg)
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
