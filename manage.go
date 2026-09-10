// manage.go implements the optional management API: an HTTP server on
// its own Unix socket (management_socket: in config.yaml) that lets
// bridges be added and removed while tsbridge keeps running, instead of
// editing config.yaml and restarting.
//
// Every bridge tsbridge runs -- whether it came from config.yaml's
// bridges: list or was added through this API -- lives in one
// bridgeManager registry, tagged with its origin ("config" or
// "managed"). Only "managed" bridges are ever written back to disk, to
// managed-bridges.yaml in state_dir: a second file in the exact same
// shape as config.yaml's own bridges: list ("just another file to
// source bridge configuration from"), merged in on top of config.yaml's
// static list at startup. config.yaml itself is never read back or
// rewritten by this API.
//
// A bridge removed through the API stops immediately and, if it was
// "managed", is dropped from managed-bridges.yaml too. A bridge that
// came from config.yaml can also be removed at runtime (it just stops
// running); since config.yaml is never rewritten, it comes back on the
// next restart unless config.yaml is edited to match.
//
// A bridge that dies of a fatal error (see bridge.go's acceptLoop/http
// server error paths) is removed from the live registry only -- not from
// managed-bridges.yaml -- so a later restart gets a fresh attempt at it.
// This is the "failure isolation" behavior: one bridge dying no longer
// takes the rest of the process down with it (see main.go), so losing
// its bookkeeping here is the full extent of the blast radius.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"gopkg.in/yaml.v3"
)

// apiError carries an HTTP status code alongside an error message, so
// management API handlers can report the right status without
// string-matching error text.
type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func badRequest(format string, a ...any) error {
	return &apiError{http.StatusBadRequest, fmt.Sprintf(format, a...)}
}

func conflictErr(format string, a ...any) error {
	return &apiError{http.StatusConflict, fmt.Sprintf(format, a...)}
}

func notFoundErr(format string, a ...any) error {
	return &apiError{http.StatusNotFound, fmt.Sprintf(format, a...)}
}

// bridgeInfo is the management API's JSON view of a running bridge --
// BridgeConfig plus Source, which has no place in a bridges: list itself
// (config.yaml and managed-bridges.yaml don't tag their own entries;
// bridgeManager does, based on which file/request a bridge came from).
type bridgeInfo struct {
	Name        string `json:"name"`
	Listen      string `json:"listen"`
	Target      string `json:"target"`
	Mode        string `json:"mode"`
	RewriteHost bool   `json:"rewrite_host"`
	// Source is "config" (loaded from config.yaml's bridges: list at
	// startup) or "managed" (added through this API, or loaded from
	// managed-bridges.yaml at startup).
	Source string `json:"source"`
}

// managedEntry is one bridge in bridgeManager's live registry.
type managedEntry struct {
	cfg    BridgeConfig
	rb     runningBridge
	source string // "config" or "managed"
	// fatalDone is closed by Remove/Shutdown when this entry is
	// deliberately stopped, so its watchFatal goroutine (below) can stop
	// waiting on a fatal error that will now never come.
	fatalDone chan struct{}
}

// bridgeManager owns every running bridge -- both the ones config.yaml
// started at boot and the ones added since through the management API --
// and the managed-bridges.yaml file that persists the latter.
type bridgeManager struct {
	ctx       context.Context
	dial      dialFunc
	sockMode  os.FileMode
	group     string
	statePath string // state_dir/managed-bridges.yaml; "" if state_dir is unset

	mu      sync.Mutex
	entries map[string]*managedEntry
}

func newBridgeManager(ctx context.Context, dial dialFunc, sockMode os.FileMode, group, statePath string) *bridgeManager {
	return &bridgeManager{
		ctx:       ctx,
		dial:      dial,
		sockMode:  sockMode,
		group:     group,
		statePath: statePath,
		entries:   make(map[string]*managedEntry),
	}
}

// startAll loads managed-bridges.yaml (if statePath is set), validates it
// together with staticBridges (config.yaml's list) as one combined set --
// same duplicate-name/listen rules as config.yaml alone -- and starts
// every bridge in it. A validation failure here is fatal to startup, the
// same as a bad config.yaml today. A bridge that fails to *start* (a bad
// listen path, e.g.) is logged and skipped rather than failing startup,
// matching the existing per-bridge tolerance.
//
// It returns how many bridges are running against how many were
// attempted, so main can decide whether "zero running" is fatal.
func (m *bridgeManager) startAll(staticBridges []BridgeConfig) (started, attempted int, err error) {
	managed, err := loadManagedBridges(m.statePath)
	if err != nil {
		return 0, 0, fmt.Errorf("loading %s: %w", m.statePath, err)
	}

	all := make([]BridgeConfig, 0, len(staticBridges)+len(managed))
	all = append(all, staticBridges...)
	all = append(all, managed...)
	if err := checkBridges(all); err != nil {
		return 0, 0, fmt.Errorf("validating bridges (config.yaml + %s): %w", m.statePath, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range staticBridges {
		if m.startLocked(b, "config") {
			started++
		}
		attempted++
	}
	for _, b := range managed {
		if m.startLocked(b, "managed") {
			started++
		}
		attempted++
	}
	return started, attempted, nil
}

// startLocked starts cfg and, on success, registers it under source.
// Callers must hold m.mu. Reports success via return value rather than
// error: a failed bridge is logged and skipped, never fatal here (see
// startAll and Add for why each caller treats that differently).
func (m *bridgeManager) startLocked(cfg BridgeConfig, source string) bool {
	fatalCh := make(chan error, 1)
	rb, err := startBridge(m.ctx, m.dial, cfg, m.sockMode, m.group, fatalCh)
	if err != nil {
		log.Printf("bridge %s: failed to start, skipping: %v", cfg.Name, err)
		return false
	}
	done := make(chan struct{})
	m.entries[cfg.Name] = &managedEntry{cfg: cfg, rb: rb, source: source, fatalDone: done}
	go m.watchFatal(cfg.Name, fatalCh, done)
	return true
}

// watchFatal removes name from the registry if it ever reports a fatal
// error, without touching managed-bridges.yaml (see the package doc
// comment). It exits without doing anything if done closes first, which
// means the entry was already removed deliberately (Remove, or process
// shutdown).
func (m *bridgeManager) watchFatal(name string, fatalCh <-chan error, done <-chan struct{}) {
	select {
	case err := <-fatalCh:
		m.mu.Lock()
		if _, ok := m.entries[name]; ok {
			delete(m.entries, name)
			log.Printf("management: bridge %q removed from the running set after a fatal error: %v", name, err)
		}
		m.mu.Unlock()
	case <-done:
	}
}

// Add validates and starts a new bridge, registers it with source
// "managed", and persists it to managed-bridges.yaml. A relative Listen
// path is rejected -- unlike config.yaml's bridges:, there's no config
// file directory to sensibly resolve one against here.
func (m *bridgeManager) Add(cfg BridgeConfig) (BridgeConfig, error) {
	normalizeBridgeMode(&cfg)
	if err := validateBridgeFields(cfg); err != nil {
		return BridgeConfig{}, badRequest("%s", err)
	}
	if !filepath.IsAbs(cfg.Listen) {
		return BridgeConfig{}, badRequest("bridge %q: listen path must be absolute when added through the management API (got %q)", cfg.Name, cfg.Listen)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.entries[cfg.Name]; exists {
		return BridgeConfig{}, conflictErr("bridge %q already exists", cfg.Name)
	}
	for _, e := range m.entries {
		if e.cfg.Listen == cfg.Listen {
			return BridgeConfig{}, conflictErr("listen path %q is already in use by bridge %q", cfg.Listen, e.cfg.Name)
		}
	}

	fatalCh := make(chan error, 1)
	rb, err := startBridge(m.ctx, m.dial, cfg, m.sockMode, m.group, fatalCh)
	if err != nil {
		return BridgeConfig{}, badRequest("starting bridge %q: %v", cfg.Name, err)
	}
	done := make(chan struct{})
	m.entries[cfg.Name] = &managedEntry{cfg: cfg, rb: rb, source: "managed", fatalDone: done}
	go m.watchFatal(cfg.Name, fatalCh, done)

	if err := m.saveLocked(); err != nil {
		// The bridge is live either way; a persistence failure means it
		// won't survive a restart, which is worth shouting about but not
		// worth tearing the bridge back down for.
		log.Printf("management: bridge %q started but failed to persist to %s: %v", cfg.Name, m.statePath, err)
	}

	return cfg, nil
}

// Remove stops name and, if it was source "managed", drops it from
// managed-bridges.yaml. A bridge that came from config.yaml just stops
// running -- config.yaml isn't rewritten, so it returns on the next
// restart unless config.yaml is edited too.
func (m *bridgeManager) Remove(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.entries[name]
	if !ok {
		return notFoundErr("bridge %q not found", name)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownDrainTimeout)
	defer cancel()
	entry.rb.Shutdown(shutdownCtx)
	close(entry.fatalDone)
	delete(m.entries, name)

	if entry.source == "managed" {
		if err := m.saveLocked(); err != nil {
			log.Printf("management: bridge %q removed but failed to update %s: %v", name, m.statePath, err)
		}
	}
	return nil
}

// Get returns the current info for one running bridge.
func (m *bridgeManager) Get(name string) (bridgeInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[name]
	if !ok {
		return bridgeInfo{}, notFoundErr("bridge %q not found", name)
	}
	return toBridgeInfo(entry), nil
}

// List returns every currently running bridge, sorted by name.
func (m *bridgeManager) List() []bridgeInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]bridgeInfo, 0, len(m.entries))
	for _, entry := range m.entries {
		out = append(out, toBridgeInfo(entry))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func toBridgeInfo(e *managedEntry) bridgeInfo {
	return bridgeInfo{
		Name:        e.cfg.Name,
		Listen:      e.cfg.Listen,
		Target:      e.cfg.Target,
		Mode:        e.cfg.Mode,
		RewriteHost: e.cfg.RewriteHost,
		Source:      e.source,
	}
}

// Shutdown stops every currently running bridge, bounded by ctx, mirroring
// runningBridge.Shutdown's contract. Called once, at process shutdown.
func (m *bridgeManager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	entries := make([]*managedEntry, 0, len(m.entries))
	for _, e := range m.entries {
		entries = append(entries, e)
	}
	m.entries = make(map[string]*managedEntry)
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		go func(e *managedEntry) {
			defer wg.Done()
			e.rb.Shutdown(ctx)
			close(e.fatalDone)
		}(e)
	}
	wg.Wait()
}

// saveLocked rewrites managed-bridges.yaml from the current registry's
// "managed" entries. Callers must hold m.mu. A no-op if statePath is
// unset (config.go's LoadConfig refuses this combination when
// management_socket is set, but startAll can still be called with an
// empty statePath in tests).
func (m *bridgeManager) saveLocked() error {
	if m.statePath == "" {
		return nil
	}
	var managed []BridgeConfig
	for _, e := range m.entries {
		if e.source == "managed" {
			managed = append(managed, e.cfg)
		}
	}
	sort.Slice(managed, func(i, j int) bool { return managed[i].Name < managed[j].Name })

	data, err := yaml.Marshal(managedBridgesFile{Bridges: managed})
	if err != nil {
		return err
	}
	return atomicWriteFile(m.statePath, data, 0600)
}

// managedBridgesFile is managed-bridges.yaml's schema -- deliberately the
// same shape as config.yaml's own bridges: list.
type managedBridgesFile struct {
	Bridges []BridgeConfig `yaml:"bridges"`
}

// loadManagedBridges reads path's bridges: list, returning (nil, nil) if
// the file doesn't exist yet (the common case before the first bridge is
// ever added through the API). An empty path also returns (nil, nil).
func loadManagedBridges(path string) ([]BridgeConfig, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f managedBridgesFile
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing YAML: %w", err)
	}
	return f.Bridges, nil
}

// atomicWriteFile writes data to path by writing to a temp file in the
// same directory and renaming it over path, so a reader (or a crash
// mid-write) never sees a partial managed-bridges.yaml.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".managed-bridges-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// --- HTTP handlers ---

// Handler returns the management API's HTTP handler:
//
//	GET    /bridges       -> list every running bridge
//	POST   /bridges       -> add a bridge (body: BridgeConfig JSON)
//	GET    /bridges/{name}    -> one bridge's info
//	DELETE /bridges/{name}    -> stop and remove one bridge
func (m *bridgeManager) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /bridges", m.handleList)
	mux.HandleFunc("POST /bridges", m.handleAdd)
	mux.HandleFunc("GET /bridges/{name}", m.handleGet)
	mux.HandleFunc("DELETE /bridges/{name}", m.handleRemove)
	return mux
}

func (m *bridgeManager) handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, m.List())
}

func (m *bridgeManager) handleGet(w http.ResponseWriter, r *http.Request) {
	info, err := m.Get(r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (m *bridgeManager) handleAdd(w http.ResponseWriter, r *http.Request) {
	var cfg BridgeConfig
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		writeError(w, badRequest("invalid JSON body: %v", err))
		return
	}

	added, err := m.Add(cfg)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toBridgeInfo(&managedEntry{cfg: added, source: "managed"}))
}

func (m *bridgeManager) handleRemove(w http.ResponseWriter, r *http.Request) {
	if err := m.Remove(r.PathValue("name")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("management: encoding response: %v", err)
	}
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var ae *apiError
	if errors.As(err, &ae) {
		status = ae.status
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// startManagementServer creates the management socket at path and starts
// serving m's API on it. Like a bridge's own fatal-error handling, a
// failure once it's up is logged rather than treated as a reason to bring
// the rest of the process down -- see the package doc comment.
func startManagementServer(path string, mode os.FileMode, group string, m *bridgeManager) (runningBridge, error) {
	l, err := createUnixSocket(path, mode, group)
	if err != nil {
		return nil, fmt.Errorf("management socket: %w", err)
	}

	srv := &http.Server{
		Handler:           m.Handler(),
		ReadHeaderTimeout: httpReadHeaderTimeout,
	}

	go func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) && m.ctx.Err() == nil {
			log.Printf("management: server stopped: %v", err)
		}
	}()

	return &httpBridge{srv: srv, l: l}, nil
}
