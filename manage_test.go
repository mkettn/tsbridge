package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
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
// with its state file under a fresh temp directory, and a context canceled
// on test cleanup.
func newTestManager(t *testing.T) (*bridgeManager, string) {
	t.Helper()
	dir := t.TempDir()
	statePath := filepath.Join(dir, managedBridgesFileName)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return newBridgeManager(ctx, failDial, 0660, "", statePath, "", fakeStatus), dir
}

// blockingBridge is a runningBridge whose Shutdown blocks until release is
// closed, standing in for a real bridge with a slow drain -- lets tests
// exercise bridgeManager's locking around a long-running Shutdown without
// an actual multi-second sleep.
type blockingBridge struct {
	release chan struct{}
}

func (b *blockingBridge) Shutdown(ctx context.Context) {
	select {
	case <-b.release:
	case <-ctx.Done():
	}
}

func TestBridgeManager_AddListGetRemove(t *testing.T) {
	m, dir := newTestManager(t)
	sockPath := filepath.Join(dir, "svc.sock")

	added, err := m.Add(BridgeConfig{Name: "svc", Listen: sockPath, Target: "example.invalid:1"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if added.Mode != "tcp" {
		t.Errorf("want mode to default to tcp, got %q", added.Mode)
	}

	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("socket not created: %v", err)
	}

	list := m.List()
	if len(list) != 1 || list[0].Name != "svc" || list[0].Source != "managed" {
		t.Fatalf("unexpected List: %+v", list)
	}

	got, err := m.Get("svc")
	if err != nil || got.Listen != sockPath {
		t.Fatalf("Get: %+v, %v", got, err)
	}

	if err := m.Remove("svc"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(m.List()) != 0 {
		t.Errorf("want empty registry after Remove")
	}
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("want socket removed after Remove, stat err = %v", err)
	}
	if _, err := m.Get("svc"); err == nil {
		t.Error("want error getting a removed bridge")
	}
}

func TestBridgeManager_AddRejectsRelativeListen(t *testing.T) {
	m, _ := newTestManager(t)
	_, err := m.Add(BridgeConfig{Name: "svc", Listen: "relative/path.sock", Target: "example.invalid:1"})
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("want error about absolute path, got: %v", err)
	}
}

func TestBridgeManager_AddRejectsDuplicateName(t *testing.T) {
	m, dir := newTestManager(t)
	cfg := BridgeConfig{Name: "svc", Listen: filepath.Join(dir, "a.sock"), Target: "example.invalid:1"}
	if _, err := m.Add(cfg); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	cfg.Listen = filepath.Join(dir, "b.sock")
	_, err := m.Add(cfg)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("want duplicate-name error, got: %v", err)
	}
}

func TestBridgeManager_AddRejectsDuplicateListen(t *testing.T) {
	m, dir := newTestManager(t)
	sockPath := filepath.Join(dir, "shared.sock")
	if _, err := m.Add(BridgeConfig{Name: "a", Listen: sockPath, Target: "example.invalid:1"}); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	_, err := m.Add(BridgeConfig{Name: "b", Listen: sockPath, Target: "example.invalid:2"})
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("want duplicate-listen error, got: %v", err)
	}
}

func TestBridgeManager_AddRejectsInvalidFields(t *testing.T) {
	m, _ := newTestManager(t)
	_, err := m.Add(BridgeConfig{Name: "svc", Listen: "/tmp/x.sock", Target: "t:1", Mode: "udp"})
	if err == nil || !strings.Contains(err.Error(), "unsupported mode") {
		t.Fatalf("want unsupported-mode error, got: %v", err)
	}
}

func TestBridgeManager_PersistsAndReloadsAcrossRestart(t *testing.T) {
	m1, dir := newTestManager(t)
	statePath := filepath.Join(dir, managedBridgesFileName)
	sockPath := filepath.Join(dir, "svc.sock")

	if _, err := m1.Add(BridgeConfig{Name: "svc", Listen: sockPath, Target: "example.invalid:1", Mode: "http"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("reading persisted state: %v", err)
	}
	if !strings.Contains(string(data), "name: svc") {
		t.Fatalf("persisted file missing bridge, got:\n%s", data)
	}

	// Simulate a restart: shut the first manager down (frees the socket)
	// and start a fresh one against the same state file.
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m1.Shutdown(shutCtx)

	m2, _ := newTestManagerWithState(t, statePath)
	started, attempted, err := m2.startAll(nil)
	if err != nil {
		t.Fatalf("startAll: %v", err)
	}
	if started != 1 || attempted != 1 {
		t.Fatalf("want 1/1 started, got %d/%d", started, attempted)
	}

	list := m2.List()
	if len(list) != 1 || list[0].Name != "svc" || list[0].Mode != "http" || list[0].Source != "managed" {
		t.Fatalf("unexpected reloaded bridge: %+v", list)
	}
}

func newTestManagerWithState(t *testing.T, statePath string) (*bridgeManager, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return newBridgeManager(ctx, failDial, 0660, "", statePath, "", fakeStatus), filepath.Dir(statePath)
}

// A managed-bridges.yaml entry that collides with a config.yaml one is
// skipped (logged), not a fatal startup error -- unlike config.yaml
// itself, that file can drift or be hand-edited, so one bad entry in it
// shouldn't take every other bridge down. config.yaml's own bridges:
// list is still fully fatal-validated, unchanged.
func TestBridgeManager_StartAllSkipsManagedBridgeCollidingWithConfig(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, managedBridgesFileName)
	managedYAML := "bridges:\n  - name: dup\n    listen: " + filepath.Join(dir, "dup.sock") + "\n    target: t:1\n    mode: tcp\n"
	if err := os.WriteFile(statePath, []byte(managedYAML), 0600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := newBridgeManager(ctx, failDial, 0660, "", statePath, "", fakeStatus)

	static := []BridgeConfig{{Name: "dup", Listen: filepath.Join(dir, "other.sock"), Target: "t:2", Mode: "tcp"}}
	started, attempted, err := m.startAll(static)
	if err != nil {
		t.Fatalf("startAll: %v", err)
	}
	if started != 1 || attempted != 1 {
		t.Fatalf("want the config-sourced bridge to start and the colliding managed one to be skipped, got %d/%d", started, attempted)
	}
	list := m.List()
	if len(list) != 1 || list[0].Name != "dup" || list[0].Source != "config" || list[0].Target != "t:2" {
		t.Fatalf("want only the config-sourced bridge running, got: %+v", list)
	}
}

// config.yaml's own bridges: list stays fully fatal-validated -- a
// duplicate within it, with no managed-bridges.yaml involved at all, is
// still a startup error naming the conflict.
func TestBridgeManager_StartAllStillFatalOnDuplicateWithinConfig(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := newBridgeManager(ctx, failDial, 0660, "", filepath.Join(dir, managedBridgesFileName), "", fakeStatus)

	static := []BridgeConfig{
		{Name: "dup", Listen: filepath.Join(dir, "a.sock"), Target: "t:1", Mode: "tcp"},
		{Name: "dup", Listen: filepath.Join(dir, "b.sock"), Target: "t:2", Mode: "tcp"},
	}
	_, _, err := m.startAll(static)
	if err == nil || !strings.Contains(err.Error(), "duplicate bridge name") {
		t.Fatalf("want duplicate-name error naming the conflict, got: %v", err)
	}
}

func TestBridgeManager_RemoveConfigSourcedBridgeLeavesStateFileUntouched(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, managedBridgesFileName)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := newBridgeManager(ctx, failDial, 0660, "", statePath, "", fakeStatus)

	static := []BridgeConfig{{Name: "static-a", Listen: filepath.Join(dir, "static.sock"), Target: "t:1", Mode: "tcp"}}
	if _, _, err := m.startAll(static); err != nil {
		t.Fatalf("startAll: %v", err)
	}
	if _, err := m.Add(BridgeConfig{Name: "managed-a", Listen: filepath.Join(dir, "managed.sock"), Target: "t:2"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("reading state file: %v", err)
	}

	if err := m.Remove("static-a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("reading state file after remove: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("removing a config-sourced bridge changed the managed state file:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if len(m.List()) != 1 || m.List()[0].Name != "managed-a" {
		t.Errorf("want only managed-a left running, got: %+v", m.List())
	}
}

// A fatal error marks the entry not-running (with the error recorded) but
// leaves it in the registry -- unlike the old behavior, which deleted it
// outright, that meant it silently vanished from managed-bridges.yaml the
// next time anything else changed (see the next test).
func TestBridgeManager_FatalMarksDeadButKeepsEntry(t *testing.T) {
	m, dir := newTestManager(t)
	sockPath := filepath.Join(dir, "svc.sock")
	cfg := BridgeConfig{Name: "svc", Listen: sockPath, Target: "example.invalid:1", Mode: "tcp"}

	// Start the bridge and register it exactly like startLocked/Add
	// would, but keep our own handle on fatalCh so the test can trigger
	// it -- simulating a real fatal accept/serve error without needing
	// to engineer one out of a real listener.
	fatalCh := make(chan error, 1)
	rb, err := startBridge(m.ctx, m.dial, cfg, m.sockMode, m.group, fatalCh)
	if err != nil {
		t.Fatalf("startBridge: %v", err)
	}
	done := make(chan struct{})
	entry := &managedEntry{cfg: cfg, rb: rb, source: "managed", fatalDone: done}
	m.mu.Lock()
	m.entries["svc"] = entry
	if err := m.saveLocked(); err != nil {
		m.mu.Unlock()
		t.Fatalf("saveLocked: %v", err)
	}
	m.mu.Unlock()
	go m.watchFatal(entry, fatalCh, done)

	before, err := os.ReadFile(m.statePath)
	if err != nil {
		t.Fatalf("reading state file: %v", err)
	}

	fatalCh <- errors.New("simulated fatal error")

	waitFor(t, func() bool {
		list := m.List()
		return len(list) == 1 && !list[0].Running
	})

	list := m.List()
	if len(list) != 1 || list[0].Name != "svc" || list[0].Running || list[0].Error == "" {
		t.Fatalf("want svc still listed, not running, with an error message: %+v", list)
	}

	after, err := os.ReadFile(m.statePath)
	if err != nil {
		t.Fatalf("reading state file after fatal: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("a fatal error rewrote the managed state file:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// watchFatal only marks the entry dead; it doesn't call rb.Shutdown
	// (mirroring a genuinely fatal bridge, which closes its own listener
	// before ever writing to the fatal channel). Clean up directly so
	// the test doesn't leak the socket.
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rb.Shutdown(shutCtx)
}

// The bug TestBridgeManager_FatalMarksDeadButKeepsEntry guards against:
// with the old delete-on-fatal behavior, a dead managed bridge would
// vanish from managed-bridges.yaml the moment anything *else* changed,
// since saveLocked rebuilds the file from the live registry. It should
// survive an unrelated write untouched.
func TestBridgeManager_DeadManagedBridgeSurvivesUnrelatedWrite(t *testing.T) {
	m, dir := newTestManager(t)

	deadCfg := BridgeConfig{Name: "dead", Listen: filepath.Join(dir, "dead.sock"), Target: "t:1", Mode: "tcp"}
	fatalCh := make(chan error, 1)
	rb, err := startBridge(m.ctx, m.dial, deadCfg, m.sockMode, m.group, fatalCh)
	if err != nil {
		t.Fatalf("startBridge: %v", err)
	}
	done := make(chan struct{})
	entry := &managedEntry{cfg: deadCfg, rb: rb, source: "managed", fatalDone: done}
	m.mu.Lock()
	m.entries["dead"] = entry
	if err := m.saveLocked(); err != nil {
		m.mu.Unlock()
		t.Fatalf("saveLocked: %v", err)
	}
	m.mu.Unlock()
	go m.watchFatal(entry, fatalCh, done)
	fatalCh <- errors.New("simulated fatal error")
	waitFor(t, func() bool {
		list := m.List()
		return len(list) == 1 && !list[0].Running
	})

	// An unrelated Add, followed by its own Remove, each rewrite the
	// state file via saveLocked -- "dead" must survive both.
	if _, err := m.Add(BridgeConfig{Name: "other", Listen: filepath.Join(dir, "other.sock"), Target: "t:2"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := m.Remove("other"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	data, err := os.ReadFile(m.statePath)
	if err != nil {
		t.Fatalf("reading state file: %v", err)
	}
	if !strings.Contains(string(data), "name: dead") {
		t.Fatalf("dead bridge was dropped from the state file by an unrelated write:\n%s", data)
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rb.Shutdown(shutCtx)
}

// The bug this guards against: watchFatal used to key on name alone. A
// Remove immediately followed by an Add of the same name (very plausible
// as an operator's "replace this bridge" workflow) would let the *old*
// bridge's fatal signal -- e.g. its listener closing during Shutdown,
// before the ctx-cancellation fix in bridge.go -- delete whatever the new
// entry with that name now is, even though it's a different, healthy
// bridge. watchFatal now compares entry identity, not just the name.
func TestBridgeManager_WatchFatalIgnoresStaleSignalAfterNameReuse(t *testing.T) {
	m, _ := newTestManager(t)

	oldFatalCh := make(chan error, 1)
	oldEntry := &managedEntry{
		cfg:       BridgeConfig{Name: "svc", Listen: "/old.sock", Target: "t:1", Mode: "tcp"},
		rb:        &blockingBridge{release: make(chan struct{})},
		source:    "managed",
		fatalDone: make(chan struct{}),
	}
	m.mu.Lock()
	m.entries["svc"] = oldEntry
	m.mu.Unlock()
	go m.watchFatal(oldEntry, oldFatalCh, oldEntry.fatalDone)

	// Simulate a Remove-then-Add cycle of the same name racing ahead of
	// the old watcher: a brand new, healthy entry now owns "svc" in the
	// registry, without oldEntry.fatalDone having been closed yet (the
	// exact window the identity check has to handle).
	newEntry := &managedEntry{
		cfg:       BridgeConfig{Name: "svc", Listen: "/new.sock", Target: "t:2", Mode: "tcp"},
		rb:        &blockingBridge{release: make(chan struct{})},
		source:    "managed",
		fatalDone: make(chan struct{}),
	}
	m.mu.Lock()
	m.entries["svc"] = newEntry
	m.mu.Unlock()

	// The old bridge's fatal error arrives late.
	oldFatalCh <- errors.New("stale fatal from the old bridge")

	// Give the stale watcher goroutine a chance to (wrongly) act.
	time.Sleep(100 * time.Millisecond)

	m.mu.Lock()
	cur := m.entries["svc"]
	m.mu.Unlock()
	if cur != newEntry {
		t.Fatalf("a stale fatal signal for the old entry clobbered the new one: %+v", cur)
	}
	if cur.rb == nil {
		t.Fatal("new entry was incorrectly marked dead by the old bridge's stale fatal signal")
	}
}

// waitFor polls cond until it's true or 2 seconds pass, failing the test
// on timeout. Used for assertions that depend on a background goroutine
// (watchFatal) having run.
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

func TestManagementHandler_ListAddGetDelete(t *testing.T) {
	m, dir := newTestManager(t)
	h := m.Handler()

	// Empty list.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/bridges", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /bridges: want 200, got %d", rec.Code)
	}
	var list []bridgeInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decoding list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("want empty list, got %+v", list)
	}

	// Add.
	body, _ := json.Marshal(BridgeConfig{Name: "svc", Listen: filepath.Join(dir, "svc.sock"), Target: "t:1"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/bridges", bytes.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /bridges: want 201, got %d: %s", rec.Code, rec.Body)
	}
	var created bridgeInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decoding created: %v", err)
	}
	if created.Name != "svc" || created.Source != "managed" || created.Mode != "tcp" {
		t.Fatalf("unexpected created bridge: %+v", created)
	}

	// Duplicate add -> 409.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/bridges", bytes.NewReader(body)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate POST /bridges: want 409, got %d", rec.Code)
	}

	// Invalid JSON -> 400.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/bridges", strings.NewReader("{not json")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid JSON POST /bridges: want 400, got %d", rec.Code)
	}

	// Get by name.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/bridges/svc", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /bridges/svc: want 200, got %d", rec.Code)
	}

	// Get missing -> 404.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/bridges/missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /bridges/missing: want 404, got %d", rec.Code)
	}

	// Delete.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("DELETE", "/bridges/svc", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /bridges/svc: want 204, got %d", rec.Code)
	}

	// Delete again -> 404.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("DELETE", "/bridges/svc", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("repeat DELETE /bridges/svc: want 404, got %d", rec.Code)
	}
}

func TestStartManagementServer_ServesOverUnixSocket(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := newBridgeManager(ctx, failDial, 0660, "", filepath.Join(dir, managedBridgesFileName), sockPath, fakeStatus)
	rb, err := startManagementServer(sockPath, 0600, "", m)
	if err != nil {
		t.Fatalf("startManagementServer: %v", err)
	}
	defer shutdown(t, cancel, rb)

	client := unixHTTPClient(sockPath)
	body, _ := json.Marshal(BridgeConfig{Name: "svc", Listen: filepath.Join(dir, "svc.sock"), Target: "t:1"})
	resp, err := client.Post("http://unix/bridges", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("want 201, got %d", resp.StatusCode)
	}

	resp2, err := client.Get("http://unix/bridges")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp2.Body.Close()
	var list []bridgeInfo
	if err := json.NewDecoder(resp2.Body).Decode(&list); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(list) != 1 || list[0].Name != "svc" {
		t.Fatalf("unexpected list over the wire: %+v", list)
	}
}

func TestLoadManagedBridges_MissingFileReturnsNil(t *testing.T) {
	bridges, err := loadManagedBridges(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("loadManagedBridges: %v", err)
	}
	if bridges != nil {
		t.Errorf("want nil for a missing file, got %+v", bridges)
	}
}

func TestAtomicWriteFile_ReplacesExistingContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.yaml")
	if err := atomicWriteFile(path, []byte("first"), 0600); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := atomicWriteFile(path, []byte("second"), 0600); err != nil {
		t.Fatalf("second write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "second" {
		t.Errorf("want %q, got %q", "second", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("want mode 0600, got %v", info.Mode().Perm())
	}
}

// Remove used to hold m.mu for its whole call, including the (up to
// shutdownDrainTimeout) wait for the bridge's in-flight connections to
// finish draining -- so removing one slow bridge would block every other
// request (List, Add, another Remove) for as long as the drain took.
// Remove now unregisters and persists under the lock, then drains
// outside it.
func TestBridgeManager_RemoveDoesNotBlockOtherCallsDuringDrain(t *testing.T) {
	m, dir := newTestManager(t)

	release := make(chan struct{})
	entry := &managedEntry{
		cfg:       BridgeConfig{Name: "slow", Listen: filepath.Join(dir, "slow.sock"), Target: "t:1", Mode: "tcp"},
		rb:        &blockingBridge{release: release},
		source:    "managed",
		fatalDone: make(chan struct{}),
	}
	m.mu.Lock()
	m.entries["slow"] = entry
	if err := m.saveLocked(); err != nil {
		m.mu.Unlock()
		t.Fatalf("saveLocked: %v", err)
	}
	m.mu.Unlock()

	removeDone := make(chan struct{})
	go func() {
		defer close(removeDone)
		if err := m.Remove("slow"); err != nil {
			t.Errorf("Remove: %v", err)
		}
	}()

	// Give Remove a moment to start (and, pre-fix, to be holding m.mu
	// for the whole drain).
	time.Sleep(50 * time.Millisecond)

	otherDone := make(chan struct{})
	go func() {
		defer close(otherDone)
		m.List()
		if _, err := m.Add(BridgeConfig{Name: "other", Listen: filepath.Join(dir, "other.sock"), Target: "t:2"}); err != nil {
			t.Errorf("Add while slow Remove was draining: %v", err)
		}
	}()

	select {
	case <-otherDone:
	case <-time.After(2 * time.Second):
		t.Fatal("List/Add blocked while Remove was draining a slow bridge")
	}

	close(release) // let the slow bridge's Shutdown, and Remove, finish
	select {
	case <-removeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Remove never returned after its bridge finished draining")
	}
}

// Neither Add nor a reloaded managed-bridges.yaml entry may claim the
// management socket's own listen path.
func TestBridgeManager_AddRejectsManagementSocketPath(t *testing.T) {
	dir := t.TempDir()
	mgmtSock := filepath.Join(dir, "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := newBridgeManager(ctx, failDial, 0660, "", filepath.Join(dir, managedBridgesFileName), mgmtSock, fakeStatus)

	_, err := m.Add(BridgeConfig{Name: "svc", Listen: mgmtSock, Target: "t:1"})
	if err == nil || !strings.Contains(err.Error(), "management socket") {
		t.Fatalf("want error naming the management socket collision, got: %v", err)
	}
}

func TestBridgeManager_StartAllSkipsManagedEntryCollidingWithManagementSocket(t *testing.T) {
	dir := t.TempDir()
	mgmtSock := filepath.Join(dir, "control.sock")
	statePath := filepath.Join(dir, managedBridgesFileName)
	managedYAML := "bridges:\n  - name: svc\n    listen: " + mgmtSock + "\n    target: t:1\n"
	if err := os.WriteFile(statePath, []byte(managedYAML), 0600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := newBridgeManager(ctx, failDial, 0660, "", statePath, mgmtSock, fakeStatus)

	started, attempted, err := m.startAll(nil)
	if err != nil {
		t.Fatalf("startAll: %v", err)
	}
	if started != 0 || attempted != 0 {
		t.Fatalf("want the colliding managed entry skipped entirely, got %d/%d", started, attempted)
	}
	if len(m.List()) != 0 {
		t.Fatalf("want nothing running, got: %+v", m.List())
	}
}

// Entries loaded from managed-bridges.yaml go through the same
// normalization LoadConfig applies to config.yaml's bridges: (mode
// lowercased/defaulted) before validation -- a hand-edited file that
// omits mode:, legal in config.yaml, must be legal here too rather than
// tripping validateBridgeFields' unsupported-mode check on empty and, pre-fix,
// taking the whole startup down over it. A genuinely invalid entry (bad
// mode, or a relative listen path -- never valid here, since there's no
// config file directory to resolve one against) is instead skipped with
// a log, and doesn't stop the rest of the file from starting.
func TestBridgeManager_StartAllNormalizesAndToleratesBadManagedEntries(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, managedBridgesFileName)
	managedYAML := `bridges:
  - name: good
    listen: ` + filepath.Join(dir, "good.sock") + `
    target: t:1
  - name: bad-mode
    listen: ` + filepath.Join(dir, "bad.sock") + `
    target: t:2
    mode: udp
  - name: relative-listen
    listen: relative.sock
    target: t:3
`
	if err := os.WriteFile(statePath, []byte(managedYAML), 0600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := newBridgeManager(ctx, failDial, 0660, "", statePath, "", fakeStatus)

	started, attempted, err := m.startAll(nil)
	if err != nil {
		t.Fatalf("startAll: %v", err)
	}
	if started != 1 || attempted != 1 {
		t.Fatalf("want only the one valid entry started, got %d/%d", started, attempted)
	}
	list := m.List()
	if len(list) != 1 || list[0].Name != "good" || list[0].Mode != "tcp" {
		t.Fatalf("want good (with mode: defaulted to tcp) running, got: %+v", list)
	}
}

func boolPtr(b bool) *bool { return &b }

// A bridge with enabled: false (in config.yaml or managed-bridges.yaml)
// is registered -- visible via GET /bridges, occupying its listen path
// -- but startAll never calls startBridge for it, so it has no socket
// and isn't counted as "attempted".
func TestBridgeManager_StartAllSkipsDisabledBridges(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := newBridgeManager(ctx, failDial, 0660, "", filepath.Join(dir, managedBridgesFileName), "", fakeStatus)

	sockPath := filepath.Join(dir, "off.sock")
	static := []BridgeConfig{{Name: "off", Listen: sockPath, Target: "t:1", Mode: "tcp", Enabled: boolPtr(false)}}
	started, attempted, err := m.startAll(static)
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

func TestBridgeManager_DisableStopsAndPersists(t *testing.T) {
	m, dir := newTestManager(t)
	sockPath := filepath.Join(dir, "svc.sock")

	if _, err := m.Add(BridgeConfig{Name: "svc", Listen: sockPath, Target: "t:1"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := m.Disable("svc"); err != nil {
		t.Fatalf("Disable: %v", err)
	}

	got, err := m.Get("svc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Enabled || got.Running || got.Error != "" {
		t.Fatalf("want disabled/not-running/no-error, got: %+v", got)
	}
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("want socket removed after Disable, stat err = %v", err)
	}

	data, err := os.ReadFile(m.statePath)
	if err != nil {
		t.Fatalf("reading state file: %v", err)
	}
	if !strings.Contains(string(data), "enabled: false") {
		t.Fatalf("want disabled state persisted, got:\n%s", data)
	}

	// Idempotent.
	if err := m.Disable("svc"); err != nil {
		t.Fatalf("second Disable: %v", err)
	}
}

func TestBridgeManager_EnableRestartsDisabledBridge(t *testing.T) {
	m, dir := newTestManager(t)
	sockPath := filepath.Join(dir, "svc.sock")

	if _, err := m.Add(BridgeConfig{Name: "svc", Listen: sockPath, Target: "t:1", Enabled: boolPtr(false)}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Fatalf("want no socket for a bridge added disabled, stat err = %v", err)
	}
	before, err := m.Get("svc")
	if err != nil || before.Enabled || before.Running {
		t.Fatalf("want disabled/not-running right after Add: %+v, %v", before, err)
	}

	if err := m.Enable("svc"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("want socket created after Enable: %v", err)
	}
	after, err := m.Get("svc")
	if err != nil || !after.Enabled || !after.Running {
		t.Fatalf("want enabled/running after Enable: %+v, %v", after, err)
	}

	// Idempotent.
	if err := m.Enable("svc"); err != nil {
		t.Fatalf("second Enable: %v", err)
	}
}

// Enable also serves as "retry": a bridge that's enabled but crashed
// (Running false with an Error) should be attempted again, not treated
// as a no-op just because Enabled was already true.
func TestBridgeManager_EnableRetriesCrashedBridge(t *testing.T) {
	m, dir := newTestManager(t)
	sockPath := filepath.Join(dir, "svc.sock")
	cfg := BridgeConfig{Name: "svc", Listen: sockPath, Target: "t:1", Mode: "tcp"}

	fatalCh := make(chan error, 1)
	rb, err := startBridge(m.ctx, m.dial, cfg, m.sockMode, m.group, fatalCh)
	if err != nil {
		t.Fatalf("startBridge: %v", err)
	}
	done := make(chan struct{})
	entry := &managedEntry{cfg: cfg, rb: rb, source: "managed", fatalDone: done}
	m.mu.Lock()
	m.entries["svc"] = entry
	m.mu.Unlock()
	go m.watchFatal(entry, fatalCh, done)
	fatalCh <- errors.New("simulated crash")
	waitFor(t, func() bool { info, _ := m.Get("svc"); return !info.Running })

	crashed, _ := m.Get("svc")
	if !crashed.Enabled || crashed.Running || crashed.Error == "" {
		t.Fatalf("want enabled/not-running/error after crash: %+v", crashed)
	}

	// The injected fatalCh send above bypassed acceptLoop's real fatal
	// path, which always closes (and thus unlinks) its listener before
	// ever signaling -- so unlike a genuine crash, sockPath is still
	// actually bound here. Shut it down for real so Enable's retry can
	// rebind the path, same as it could after a real crash.
	shutCtx, cancelShut := context.WithTimeout(context.Background(), 5*time.Second)
	rb.Shutdown(shutCtx)
	cancelShut()

	if err := m.Enable("svc"); err != nil {
		t.Fatalf("Enable (retry): %v", err)
	}
	retried, err := m.Get("svc")
	if err != nil || !retried.Running || retried.Error != "" {
		t.Fatalf("want running again with no error after retry: %+v, %v", retried, err)
	}
}

func TestBridgeManager_DisableConfigSourcedBridgeLeavesStateFileUntouched(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, managedBridgesFileName)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := newBridgeManager(ctx, failDial, 0660, "", statePath, "", fakeStatus)

	static := []BridgeConfig{{Name: "static-a", Listen: filepath.Join(dir, "static.sock"), Target: "t:1", Mode: "tcp"}}
	if _, _, err := m.startAll(static); err != nil {
		t.Fatalf("startAll: %v", err)
	}

	if err := m.Disable("static-a"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("want no state file written for a config-sourced bridge's Disable, stat err = %v", err)
	}
	info, err := m.Get("static-a")
	if err != nil || info.Enabled || info.Running {
		t.Fatalf("want static-a disabled/not-running: %+v, %v", info, err)
	}
}

func TestManagementHandler_DisableEnable(t *testing.T) {
	m, dir := newTestManager(t)
	h := m.Handler()

	body, _ := json.Marshal(BridgeConfig{Name: "svc", Listen: filepath.Join(dir, "svc.sock"), Target: "t:1"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/bridges", bytes.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /bridges: want 201, got %d: %s", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/bridges/svc/disable", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: want 200, got %d: %s", rec.Code, rec.Body)
	}
	var disabled bridgeInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &disabled); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if disabled.Enabled || disabled.Running {
		t.Fatalf("want disabled/not-running in response: %+v", disabled)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/bridges/missing/disable", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disable missing: want 404, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/bridges/svc/enable", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: want 200, got %d: %s", rec.Code, rec.Body)
	}
	var enabled bridgeInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &enabled); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if !enabled.Enabled || !enabled.Running {
		t.Fatalf("want enabled/running in response: %+v", enabled)
	}
}

func TestManagementHandler_Status(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := newBridgeManager(ctx, failDial, 0660, "", filepath.Join(dir, managedBridgesFileName), "", func(ctx context.Context) (*ipnstate.Status, error) {
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
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := newBridgeManager(ctx, failDial, 0660, "", filepath.Join(dir, managedBridgesFileName), "", func(ctx context.Context) (*ipnstate.Status, error) {
		return nil, errors.New("simulated status failure")
	})

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 when the status fetch fails, got %d: %s", rec.Code, rec.Body)
	}
}
