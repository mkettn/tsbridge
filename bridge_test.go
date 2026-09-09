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
	"time"

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

	b := BridgeConfig{Name: "test", Listen: sockPath, Target: "example.invalid:1", Mode: "tcp"}

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

	b := BridgeConfig{Name: "test", Listen: path, Target: "example.invalid:1", Mode: "tcp"}
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

	b := BridgeConfig{Name: "test", Listen: sockPath, Target: "example.invalid:1", Mode: "tcp"}
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

	b := BridgeConfig{Name: "test", Listen: sockPath, Target: "example.invalid:1", Mode: "tcp"}

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

func TestIsTemporaryAcceptError(t *testing.T) {
	temp := &net.OpError{Op: "accept", Err: os.NewSyscallError("accept", syscall.EMFILE)}
	if !isTemporaryAcceptError(temp) {
		t.Error("EMFILE wrapped in *net.OpError should be temporary")
	}
	fatal := &net.OpError{Op: "accept", Err: os.NewSyscallError("accept", syscall.EINVAL)}
	if isTemporaryAcceptError(fatal) {
		t.Error("EINVAL should not be temporary")
	}
}

// fakeListener replays a scripted sequence of Accept results, so
// acceptLoop's retry/backoff/fatal decisions can be exercised without a
// real socket or a real tsnet server.
type fakeListener struct {
	mu      sync.Mutex
	results []error
	idx     int
	closed  bool
}

func (f *fakeListener) Accept() (net.Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.idx >= len(f.results) {
		select {} // scripted results exhausted; block rather than panic
	}
	err := f.results[f.idx]
	f.idx++
	return nil, err
}

func (f *fakeListener) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeListener) Addr() net.Addr { return &net.UnixAddr{Name: "fake", Net: "unix"} }

func (f *fakeListener) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.idx
}

func TestAcceptLoop_RetriesTemporaryThenReportsFatal(t *testing.T) {
	l := &fakeListener{results: []error{
		&net.OpError{Op: "accept", Err: os.NewSyscallError("accept", syscall.EMFILE)},
		&net.OpError{Op: "accept", Err: os.NewSyscallError("accept", syscall.ENFILE)},
		&net.OpError{Op: "accept", Err: os.NewSyscallError("accept", syscall.EINVAL)}, // not temporary -> fatal
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := BridgeConfig{Name: "test-bridge", Listen: "/unused", Target: "example.invalid:1", Mode: "tcp"}
	var wg sync.WaitGroup
	fatal := make(chan error, 1)

	done := make(chan struct{})
	go func() {
		acceptLoop(ctx, &tsnet.Server{}, b, l, &wg, fatal)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("acceptLoop did not return after a fatal error")
	}

	if got := l.callCount(); got != 3 {
		t.Errorf("want 3 Accept calls (2 retried + 1 fatal), got %d", got)
	}
	if !l.closed {
		t.Error("want listener closed on fatal error")
	}

	select {
	case err := <-fatal:
		if !strings.Contains(err.Error(), "test-bridge") {
			t.Errorf("fatal error should name the bridge, got: %v", err)
		}
	default:
		t.Error("want an error reported on the fatal channel")
	}
}
