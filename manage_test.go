package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tailscale.com/ipn/ipnstate"
)

// fakeStatus is a statusFunc for tests that don't care about GET
// /status's actual content -- TestManagementHandler_Status below uses
// its own to check that content specifically.
func fakeStatus(ctx context.Context) (*ipnstate.Status, error) {
	return &ipnstate.Status{BackendState: "Running"}, nil
}

// newTestManager returns a bridgeManager wired to failDial (bridge_test.go)
// and a context canceled on test cleanup, plus a fresh temp directory.
func newTestManager(t *testing.T) (*bridgeManager, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return newBridgeManager(ctx, failDial, 0660, "", fakeStatus), t.TempDir()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !cond() {
		t.Fatal("condition was never met within 2s")
	}
}

func boolPtr(b bool) *bool { return &b }

func TestBridgeManager_StartAllListsAndGets(t *testing.T) {
	m, dir := newTestManager(t)
	sockPath := filepath.Join(dir, "svc.sock")
	started, attempted, err := m.startAll([]BridgeConfig{{Name: "svc", Listen: sockPath, Targets: Targets{"example.invalid:1"}, Mode: "tcp"}})
	if err != nil {
		t.Fatalf("startAll: %v", err)
	}
	if started != 1 || attempted != 1 {
		t.Fatalf("want 1/1, got %d/%d", started, attempted)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		m.Shutdown(ctx)
	}()

	list := m.List()
	if len(list) != 1 || list[0].Name != "svc" || !list[0].Running || !list[0].Enabled || len(list[0].Target) != 1 {
		t.Fatalf("unexpected list: %+v", list)
	}
	if _, err := m.Get("svc"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := m.Get("nope"); err == nil {
		t.Error("Get of an unknown bridge should fail")
	}
}

func TestBridgeManager_StartAllFatalOnDuplicateWithinConfig(t *testing.T) {
	m, dir := newTestManager(t)
	_, _, err := m.startAll([]BridgeConfig{
		{Name: "dup", Listen: filepath.Join(dir, "a.sock"), Targets: Targets{"t:1"}, Mode: "tcp"},
		{Name: "dup", Listen: filepath.Join(dir, "b.sock"), Targets: Targets{"t:2"}, Mode: "tcp"},
	})
	if err == nil {
		t.Fatal("want startAll to reject a duplicate bridge name")
	}
}

// A bridge with enabled: false is registered -- visible via GET /bridges --
// but startAll never calls startBridge for it, so it has no socket and
// isn't counted as "attempted".
func TestBridgeManager_StartAllSkipsDisabledBridges(t *testing.T) {
	m, dir := newTestManager(t)
	sockPath := filepath.Join(dir, "off.sock")
	started, attempted, err := m.startAll([]BridgeConfig{{Name: "off", Listen: sockPath, Targets: Targets{"t:1"}, Mode: "tcp", Enabled: boolPtr(false)}})
	if err != nil {
		t.Fatalf("startAll: %v", err)
	}
	if started != 0 || attempted != 0 {
		t.Fatalf("want a disabled bridge to be neither started nor attempted, got %d/%d", started, attempted)
	}
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("want no socket for a disabled bridge, stat err = %v", err)
	}
	info, err := m.Get("off")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.Enabled || info.Running || info.Error != "" {
		t.Fatalf("want disabled/not-running/no-error, got: %+v", info)
	}
}

func TestBridgeManager_FailedStartStaysListedWithError(t *testing.T) {
	m, dir := newTestManager(t)
	// A listen path whose directory doesn't exist can't be created.
	bad := filepath.Join(dir, "missing", "svc.sock")
	started, attempted, err := m.startAll([]BridgeConfig{{Name: "bad", Listen: bad, Targets: Targets{"t:1"}, Mode: "tcp"}})
	if err != nil {
		t.Fatalf("startAll: %v", err)
	}
	if started != 0 || attempted != 1 {
		t.Fatalf("want 0/1, got %d/%d", started, attempted)
	}
	info, _ := m.Get("bad")
	if info.Running || info.Error == "" {
		t.Fatalf("want not running with an error, got %+v", info)
	}
}

func TestBridgeManager_FatalMarksDeadButKeepsEntry(t *testing.T) {
	m, dir := newTestManager(t)
	sockPath := filepath.Join(dir, "svc.sock")
	cfg := BridgeConfig{Name: "svc", Listen: sockPath, Targets: Targets{"example.invalid:1"}, Mode: "tcp"}

	// Register the bridge like startLocked would, but keep our own handle
	// on fatalCh so the test can simulate a real fatal accept/serve error.
	fatalCh := make(chan error, 1)
	rb, err := startBridge(m.ctx, m.dial, cfg, m.sockMode, m.group, fatalCh)
	if err != nil {
		t.Fatalf("startBridge: %v", err)
	}
	done := make(chan struct{})
	entry := &bridgeEntry{cfg: cfg, rb: rb, fatalDone: done}
	m.mu.Lock()
	m.entries["svc"] = entry
	m.mu.Unlock()
	go m.watchFatal(entry, fatalCh, done)

	fatalCh <- errors.New("simulated fatal error")

	waitFor(t, func() bool {
		list := m.List()
		return len(list) == 1 && !list[0].Running
	})
	list := m.List()
	if len(list) != 1 || list[0].Name != "svc" || list[0].Running || list[0].Error == "" {
		t.Fatalf("want svc still listed, not running, with an error message: %+v", list)
	}

	// watchFatal only marks the entry dead; clean up the real listener.
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rb.Shutdown(shutCtx)
}

func TestManagementHandler_ReadOnly(t *testing.T) {
	m, dir := newTestManager(t)
	if _, _, err := m.startAll([]BridgeConfig{{Name: "svc", Listen: filepath.Join(dir, "svc.sock"), Targets: Targets{"t:1"}, Mode: "tcp"}}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		m.Shutdown(ctx)
	}()
	h := m.Handler()

	do := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec
	}

	if rec := do("GET", "/bridges"); rec.Code != http.StatusOK {
		t.Fatalf("GET /bridges: %d", rec.Code)
	}
	if rec := do("GET", "/bridges/svc"); rec.Code != http.StatusOK {
		t.Fatalf("GET /bridges/svc: %d", rec.Code)
	}
	if rec := do("GET", "/bridges/nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("GET /bridges/nope: want 404, got %d", rec.Code)
	}
	for _, c := range [][2]string{
		{"POST", "/bridges"},
		{"DELETE", "/bridges/svc"},
		{"POST", "/bridges/svc/disable"},
		{"POST", "/bridges/svc/enable"},
	} {
		if rec := do(c[0], c[1]); rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: want 404/405, got %d", c[0], c[1], rec.Code)
		} else if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
			t.Errorf("%s %s: want a JSON error, got Content-Type %q", c[0], c[1], ct)
		}
	}
	if len(m.List()) != 1 {
		t.Error("a rejected mutation changed the bridge list")
	}
}

func TestStartManagementServer_ServesOverUnixSocket(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := newBridgeManager(ctx, failDial, 0660, "", fakeStatus)
	if _, _, err := m.startAll([]BridgeConfig{{Name: "svc", Listen: filepath.Join(dir, "svc.sock"), Targets: Targets{"t:1"}, Mode: "tcp"}}); err != nil {
		t.Fatal(err)
	}
	rb, err := startManagementServer(sockPath, 0600, "", m)
	if err != nil {
		t.Fatalf("startManagementServer: %v", err)
	}
	defer shutdown(t, cancel, rb)

	client := unixHTTPClient(sockPath)
	resp, err := client.Get("http://unix/bridges")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var list []bridgeInfo
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(list) != 1 || list[0].Name != "svc" {
		t.Fatalf("unexpected list over the wire: %+v", list)
	}

	resp2, err := client.Post("http://unix/bridges", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode == http.StatusCreated || resp2.StatusCode == http.StatusOK {
		t.Fatalf("POST /bridges must not be accepted, got %d", resp2.StatusCode)
	}
}

func TestManagementHandler_Status(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := newBridgeManager(ctx, failDial, 0660, "", func(ctx context.Context) (*ipnstate.Status, error) {
		return &ipnstate.Status{
			BackendState:   "Running",
			TailscaleIPs:   []netip.Addr{netip.MustParseAddr("100.64.0.1")},
			CurrentTailnet: &ipnstate.TailnetStatus{Name: "example.ts.net"},
			Health:         []string{"warning: something"},
		}, nil
	})

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /status: want 200, got %d: %s", rec.Code, rec.Body)
	}
	var got statusInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got.BackendState != "Running" || got.Tailnet != "example.ts.net" || len(got.TailscaleIPs) != 1 || got.TailscaleIPs[0] != "100.64.0.1" || len(got.Health) != 1 {
		t.Fatalf("unexpected status: %+v", got)
	}
}

func TestManagementHandler_StatusError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := newBridgeManager(ctx, failDial, 0660, "", func(ctx context.Context) (*ipnstate.Status, error) {
		return nil, errors.New("simulated status failure")
	})

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 when the status fetch fails, got %d: %s", rec.Code, rec.Body)
	}
}
