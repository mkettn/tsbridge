package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestManager returns a bridgeManager wired to failDial (bridge_test.go)
// with its state file under a fresh temp directory, and a context canceled
// on test cleanup.
func newTestManager(t *testing.T) (*bridgeManager, string) {
	t.Helper()
	dir := t.TempDir()
	statePath := filepath.Join(dir, managedBridgesFileName)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return newBridgeManager(ctx, failDial, 0660, "", statePath), dir
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
	return newBridgeManager(ctx, failDial, 0660, "", statePath), filepath.Dir(statePath)
}

func TestBridgeManager_StartAllValidatesCombinedConfigAndManagedBridges(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, managedBridgesFileName)
	managedYAML := "bridges:\n  - name: dup\n    listen: " + filepath.Join(dir, "dup.sock") + "\n    target: t:1\n    mode: tcp\n"
	if err := os.WriteFile(statePath, []byte(managedYAML), 0600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := newBridgeManager(ctx, failDial, 0660, "", statePath)

	static := []BridgeConfig{{Name: "dup", Listen: filepath.Join(dir, "other.sock"), Target: "t:2", Mode: "tcp"}}
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
	m := newBridgeManager(ctx, failDial, 0660, "", statePath)

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

func TestBridgeManager_FatalRemovesFromRegistryButNotFromStateFile(t *testing.T) {
	m, dir := newTestManager(t)
	sockPath := filepath.Join(dir, "svc.sock")
	cfg := BridgeConfig{Name: "svc", Listen: sockPath, Target: "example.invalid:1", Mode: "tcp"}

	// Start the bridge and register it exactly like startLocked/Add would,
	// but keep our own handle on fatalCh so the test can trigger it --
	// simulating a real fatal accept/serve error without needing to
	// engineer one out of a real listener.
	fatalCh := make(chan error, 1)
	rb, err := startBridge(m.ctx, m.dial, cfg, m.sockMode, m.group, fatalCh)
	if err != nil {
		t.Fatalf("startBridge: %v", err)
	}
	done := make(chan struct{})
	m.mu.Lock()
	m.entries["svc"] = &managedEntry{cfg: cfg, rb: rb, source: "managed", fatalDone: done}
	if err := m.saveLocked(); err != nil {
		m.mu.Unlock()
		t.Fatalf("saveLocked: %v", err)
	}
	m.mu.Unlock()
	go m.watchFatal("svc", fatalCh, done)

	before, err := os.ReadFile(m.statePath)
	if err != nil {
		t.Fatalf("reading state file: %v", err)
	}

	fatalCh <- errors.New("simulated fatal error")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(m.List()) != 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(m.List()) != 0 {
		t.Fatalf("want bridge removed from registry after fatal, still present: %+v", m.List())
	}

	after, err := os.ReadFile(m.statePath)
	if err != nil {
		t.Fatalf("reading state file after fatal: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("a fatal error rewrote the managed state file:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// watchFatal only drops the registry entry; it doesn't call
	// rb.Shutdown (mirroring a genuinely fatal bridge, which closes its
	// own listener before ever writing to the fatal channel). Clean up
	// directly so the test doesn't leak the socket.
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rb.Shutdown(shutCtx)
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

	m := newBridgeManager(ctx, failDial, 0660, "", filepath.Join(dir, managedBridgesFileName))
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
