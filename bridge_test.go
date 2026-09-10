package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// failDial is a dialFunc for tests that never expect to actually dial
// the tailnet target -- any call to it is a test bug.
func failDial(ctx context.Context, network, address string) (net.Conn, error) {
	return nil, fmt.Errorf("unexpected dial to %s %s", network, address)
}

// shutdown cancels ctx (the bridge's lifetime context, so acceptLoop/the
// http server treat what follows as an intentional stop) and then calls
// rb.Shutdown bounded by a short timeout, the same sequence main() uses.
func shutdown(t *testing.T, cancel context.CancelFunc, rb runningBridge) {
	t.Helper()
	cancel()
	ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	rb.Shutdown(ctx)
}

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

	fatal := make(chan error, 1)
	rb, err := startBridge(ctx, failDial, b, 0640, "", fatal)
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

	shutdown(t, cancel, rb)

	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("want socket removed after Shutdown, stat err = %v", err)
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
	fatal := make(chan error, 1)
	_, err := startBridge(ctx, failDial, b, 0660, "", fatal)
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
	fatal := make(chan error, 1)
	_, err = startBridge(ctx, failDial, b, 0660, "", fatal)
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

	fatal := make(chan error, 1)
	rb, err := startBridge(ctx, failDial, b, 0660, "root", fatal)
	if err != nil {
		t.Fatalf("startBridge: %v", err)
	}
	defer shutdown(t, cancel, rb)

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
// real socket.
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
		acceptLoop(ctx, failDial, b, l, &wg, fatal)
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

// dialToAddr returns a dialFunc that ignores whatever address it's asked
// to dial and always connects to backendAddr instead -- standing in for
// tsnet.Server.Dial reaching a fixed tailnet target in tests.
func dialToAddr(backendAddr string) dialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", backendAddr)
	}
}

// unixHTTPClient returns an *http.Client that dials sockPath for every
// request, regardless of the URL's host -- so plain "http://unix/..."
// URLs reach the bridge's Unix socket.
func unixHTTPClient(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
	}
}

// startHTTPTestBackend runs a throwaway HTTP server standing in for the
// tailnet target: it echoes the request path in the body, reports the
// Host header it saw on hostCh, and sets X-Backend so tests can confirm
// the response actually came from here.
func startHTTPTestBackend(t *testing.T) (addr string, hostCh chan string) {
	t.Helper()
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hostCh = make(chan string, 1)
	backendSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hostCh <- r.Host
		w.Header().Set("X-Backend", "yes")
		fmt.Fprintf(w, "hello from %s", r.URL.Path)
	})}
	go backendSrv.Serve(backend)
	t.Cleanup(func() {
		backendSrv.Close()
		backend.Close()
	})
	return backend.Addr().String(), hostCh
}

func TestStartHTTPBridge_ReverseProxiesToTarget(t *testing.T) {
	backendAddr, hostCh := startHTTPTestBackend(t)

	dir := t.TempDir()
	sockPath := filepath.Join(dir, "http.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := BridgeConfig{Name: "http-test", Listen: sockPath, Target: "example.invalid:80", Mode: "http"}
	fatal := make(chan error, 1)
	rb, err := startBridge(ctx, dialToAddr(backendAddr), b, 0660, "", fatal)
	if err != nil {
		t.Fatalf("startBridge: %v", err)
	}
	defer shutdown(t, cancel, rb)

	resp, err := unixHTTPClient(sockPath).Get("http://unix/some/path")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("want 200, got %d", resp.StatusCode)
	}
	if want := "hello from /some/path"; string(body) != want {
		t.Errorf("want body %q, got %q", want, body)
	}
	if resp.Header.Get("X-Backend") != "yes" {
		t.Errorf("missing backend response header, got: %v", resp.Header)
	}

	// Default (rewrite_host unset/false): the client's own Host header
	// -- "unix", from the http://unix/... URL unixHTTPClient uses --
	// passes through unchanged, not target's hostname.
	select {
	case gotHost := <-hostCh:
		if gotHost != "unix" {
			t.Errorf("backend saw Host %q, want client's original %q", gotHost, "unix")
		}
	default:
		t.Fatal("backend handler never ran")
	}

	select {
	case err := <-fatal:
		t.Errorf("unexpected fatal error: %v", err)
	default:
	}
}

func TestStartHTTPBridge_RewriteHostSetsTargetHost(t *testing.T) {
	backendAddr, hostCh := startHTTPTestBackend(t)

	dir := t.TempDir()
	sockPath := filepath.Join(dir, "http-rewrite.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := BridgeConfig{Name: "http-rewrite", Listen: sockPath, Target: "example.invalid:80", Mode: "http", RewriteHost: true}
	fatal := make(chan error, 1)
	rb, err := startBridge(ctx, dialToAddr(backendAddr), b, 0660, "", fatal)
	if err != nil {
		t.Fatalf("startBridge: %v", err)
	}
	defer shutdown(t, cancel, rb)

	resp, err := unixHTTPClient(sockPath).Get("http://unix/some/path")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	// rewrite_host: true forces target's own hostname onto the proxied
	// request, e.g. for a target that routes/validates by Host (tailscale
	// serve, notably) rather than whatever the client sent.
	select {
	case gotHost := <-hostCh:
		if gotHost != b.Target {
			t.Errorf("backend saw Host %q, want %q", gotHost, b.Target)
		}
	default:
		t.Fatal("backend handler never ran")
	}
}

func TestStartHTTPBridge_UnreachableTargetReturnsBadGateway(t *testing.T) {
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, fmt.Errorf("simulated dial failure")
	}

	dir := t.TempDir()
	sockPath := filepath.Join(dir, "http-fail.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := BridgeConfig{Name: "http-fail", Listen: sockPath, Target: "example.invalid:80", Mode: "http"}
	fatal := make(chan error, 1)
	rb, err := startBridge(ctx, dial, b, 0660, "", fatal)
	if err != nil {
		t.Fatalf("startBridge: %v", err)
	}
	defer shutdown(t, cancel, rb)

	resp, err := unixHTTPClient(sockPath).Get("http://unix/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("want 502, got %d", resp.StatusCode)
	}
}

// TestStartHTTPBridge_ShutdownAlwaysUnlinksSocket guards against
// http.Server.Shutdown returning nil (having closed nothing) when it
// races the Serve goroutine's own startup: that path would silently
// skip the Close() fallback too, since that only fires on error,
// leaving the socket file behind after a "clean" shutdown. Immediately
// shutting down right after startBridge returns -- as shutdown() does --
// puts Shutdown right in that startup race, so repeating it reliably
// exercises the window rather than winning it by chance.
func TestStartHTTPBridge_ShutdownAlwaysUnlinksSocket(t *testing.T) {
	for i := 0; i < 50; i++ {
		dir := t.TempDir()
		sockPath := filepath.Join(dir, "race.sock")
		ctx, cancel := context.WithCancel(context.Background())

		b := BridgeConfig{Name: "race", Listen: sockPath, Target: "example.invalid:80", Mode: "http"}
		fatal := make(chan error, 1)
		rb, err := startBridge(ctx, failDial, b, 0660, "", fatal)
		if err != nil {
			cancel()
			t.Fatalf("iteration %d: startBridge: %v", i, err)
		}

		shutdown(t, cancel, rb)

		if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
			t.Fatalf("iteration %d: socket file still present after Shutdown, stat err = %v", i, err)
		}
	}
}
