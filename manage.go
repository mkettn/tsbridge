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
	"time"

	"gopkg.in/yaml.v3"

	"tailscale.com/ipn/ipnstate"
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

// bridgeInfo is the management API's JSON view of a bridge -- BridgeConfig
// plus Source (which has no place in a bridges: list itself: config.yaml
// and managed-bridges.yaml don't tag their own entries, bridgeManager
// does), Enabled/Running/Error, and the bridge's dial health.
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
	// Enabled reflects operator intent, independent of whether the
	// bridge is currently Running: false means it was deliberately
	// taken offline (Disable, or enabled: false in its source file) and
	// has no socket. true with Running false instead means it's
	// supposed to be up but isn't (failed to start, or a fatal error --
	// Error explains which).
	Enabled bool   `json:"enabled"`
	Running bool   `json:"running"`
	Error   string `json:"error,omitempty"`
	// DialFailures/LastDialError/LastDialAt report Target's reachability
	// as observed by this bridge's own traffic -- tsbridge never probes
	// Target on its own, so these only update when something actually
	// connects through the bridge, and a bridge with no traffic (or one
	// that's Enabled false, or not Running) reports all zero/empty
	// regardless of Target's actual state. DialFailures counts
	// consecutive failures since the last successful dial (0 if the
	// most recent dial succeeded, or none has happened yet).
	DialFailures  int        `json:"dial_failures,omitempty"`
	LastDialError string     `json:"last_dial_error,omitempty"`
	LastDialAt    *time.Time `json:"last_dial_at,omitempty"`
}

// managedEntry is one bridge in bridgeManager's registry -- including a
// bridge that isn't currently running (rb == nil): either it failed to
// start, or it started and later hit a fatal error. It stays in the
// registry either way rather than being deleted, so managed-bridges.yaml
// (which saveLocked rebuilds from source == "managed" entries in the
// registry, regardless of rb) doesn't lose it on the next unrelated
// write, and so GET /bridges can still report it and why it's down.
// Remove is the only thing that ever deletes an entry.
type managedEntry struct {
	cfg    BridgeConfig
	rb     runningBridge // nil if not currently running
	source string        // "config" or "managed"
	// lastError explains why rb is nil, when that's due to a failure
	// (starting it, or a later fatal error) rather than never having
	// been attempted.
	lastError error
	// fatalDone is non-nil only while rb is non-nil: closed by
	// Remove/Shutdown when this entry is deliberately stopped, so its
	// watchFatal goroutine (below) can stop waiting on a fatal error
	// that will now never come.
	fatalDone chan struct{}
}

// bridgeManager owns every bridge tsbridge runs -- both the ones
// config.yaml started at boot and the ones added since through the
// management API -- and the managed-bridges.yaml file that persists the
// latter.
type bridgeManager struct {
	ctx       context.Context
	dial      dialFunc
	sockMode  os.FileMode
	group     string
	statePath string // state_dir/managed-bridges.yaml; "" if state_dir is unset
	// managementSocket is the management socket's own listen path, if
	// any ("" if management_socket is unset). No bridge -- config,
	// managed, or added through the API -- may claim it as its own
	// Listen path; see startAll and Add.
	managementSocket string
	// status fetches the tailnet connection's own current state, for
	// GET /status. Set to a func wrapping srv.LocalClient().Status by
	// main.go; tests substitute a fake.
	status statusFunc

	mu      sync.Mutex
	entries map[string]*managedEntry
}

// statusFunc fetches the tailnet connection's current status --
// tsnet.Server.LocalClient().StatusWithoutPeers in production (see
// main.go), matched by *local.Client's own method signature so main.go
// can pass it in directly with no wrapping.
type statusFunc func(ctx context.Context) (*ipnstate.Status, error)

func newBridgeManager(ctx context.Context, dial dialFunc, sockMode os.FileMode, group, statePath, managementSocket string, status statusFunc) *bridgeManager {
	return &bridgeManager{
		ctx:              ctx,
		dial:             dial,
		sockMode:         sockMode,
		group:            group,
		statePath:        statePath,
		managementSocket: managementSocket,
		status:           status,
		entries:          make(map[string]*managedEntry),
	}
}

// startAll validates and starts staticBridges (config.yaml's list) --
// same as today: a validation failure here is fatal to startup, the same
// as a bad config.yaml has always been. It then loads managed-bridges.yaml
// (if statePath is set) and starts what's valid in it too, but more
// tolerantly: unlike config.yaml, that file can be hand-edited or drift
// from what this binary's Add wrote, so a single malformed or duplicate
// entry in it is logged and skipped rather than failing startup outright
// -- one bad managed entry shouldn't take every other bridge down with
// it, consistent with the failure-isolation this manager provides at
// runtime too. Every managed entry is also normalized (mode
// lowercased/defaulted) the same way config.yaml's are, since a
// hand-edited file won't have gone through that step already, and
// checked against an absolute Listen path and the management socket's
// own path, matching Add's rules for a bridge submitted through the API.
//
// A bridge that fails to *start* (a bad listen path, e.g.) is also
// logged and skipped rather than failing startup, matching the existing
// per-bridge tolerance -- but stays in the registry as a non-running
// entry (see managedEntry) rather than vanishing, so it's visible via
// GET /bridges and, if it's "managed", isn't dropped from
// managed-bridges.yaml by a later unrelated write.
//
// It returns how many bridges ended up running against how many were
// attempted, so main can decide whether "zero running" is fatal.
func (m *bridgeManager) startAll(staticBridges []BridgeConfig) (started, attempted int, err error) {
	if err := checkBridges(staticBridges); err != nil {
		return 0, 0, err
	}

	managed, err := loadManagedBridges(m.statePath)
	if err != nil {
		return 0, 0, fmt.Errorf("loading %s: %w", m.statePath, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	names := map[string]bool{}
	listens := map[string]bool{}
	for _, b := range staticBridges {
		names[b.Name] = true
		listens[b.Listen] = true
		s, a := m.startLocked(b, "config")
		if s {
			started++
		}
		if a {
			attempted++
		}
	}

	for _, b := range managed {
		normalizeBridge(&b)
		if err := validateBridgeFields(b); err != nil {
			log.Printf("management: skipping invalid entry in %s: %v", m.statePath, err)
			continue
		}
		if !filepath.IsAbs(b.Listen) {
			log.Printf("management: skipping %q in %s: listen path must be absolute (got %q)", b.Name, m.statePath, b.Listen)
			continue
		}
		if b.Listen == m.managementSocket {
			log.Printf("management: skipping %q in %s: listen path %q collides with management_socket", b.Name, m.statePath, b.Listen)
			continue
		}
		if names[b.Name] {
			log.Printf("management: skipping %q in %s: duplicate bridge name", b.Name, m.statePath)
			continue
		}
		if listens[b.Listen] {
			log.Printf("management: skipping %q in %s: duplicate listen path %q", b.Name, m.statePath, b.Listen)
			continue
		}
		names[b.Name] = true
		listens[b.Listen] = true
		s, a := m.startLocked(b, "managed")
		if s {
			started++
		}
		if a {
			attempted++
		}
	}
	return started, attempted, nil
}

// startLocked registers cfg under source, starting it unless it's
// disabled (enabled: false, in cfg or set explicitly). Callers must hold
// m.mu. Reports outcome via return values rather than error -- a failed
// or disabled bridge is registered (see managedEntry) rather than
// treated as fatal here (see startAll and Add for why each caller treats
// a *failure* differently) -- distinguishing whether it was actually
// attempted, so startAll's caller can tell "nothing configured to run"
// from "tried and failed".
func (m *bridgeManager) startLocked(cfg BridgeConfig, source string) (started, attempted bool) {
	if cfg.Enabled != nil && !*cfg.Enabled {
		m.entries[cfg.Name] = &managedEntry{cfg: cfg, source: source}
		return false, false
	}
	fatalCh := make(chan error, 1)
	rb, err := startBridge(m.ctx, m.dial, cfg, m.sockMode, m.group, fatalCh)
	if err != nil {
		log.Printf("bridge %s: failed to start, skipping: %v", cfg.Name, err)
		m.entries[cfg.Name] = &managedEntry{cfg: cfg, source: source, lastError: err}
		return false, true
	}
	done := make(chan struct{})
	entry := &managedEntry{cfg: cfg, rb: rb, source: source, fatalDone: done}
	m.entries[cfg.Name] = entry
	go m.watchFatal(entry, fatalCh, done)
	return true, true
}

// watchFatal marks entry as no longer running if it ever reports a fatal
// error -- clearing rb and recording err in lastError, but leaving the
// entry itself in the registry (see managedEntry) rather than deleting
// it, so it stays visible and, if "managed", stays in
// managed-bridges.yaml. It exits without doing anything if done closes
// first, which means entry was already removed deliberately (Remove, or
// process shutdown).
//
// It compares identity (cur == entry), not just name, before touching
// the registry: name alone isn't enough once a name can be reused (a
// Remove immediately followed by an Add of the same name) while this
// goroutine is still waiting on fatalCh from the *previous* bridge that
// held that name -- keying on name alone would let a stale fatal signal
// delete a brand new, healthy entry.
func (m *bridgeManager) watchFatal(entry *managedEntry, fatalCh <-chan error, done <-chan struct{}) {
	select {
	case err := <-fatalCh:
		m.mu.Lock()
		if cur, ok := m.entries[entry.cfg.Name]; ok && cur == entry {
			entry.rb = nil
			entry.lastError = err
			entry.fatalDone = nil
			log.Printf("management: bridge %q stopped after a fatal error: %v", entry.cfg.Name, err)
		}
		m.mu.Unlock()
	case <-done:
	}
}

// Add validates and starts a new bridge, registers it with source
// "managed", and persists it to managed-bridges.yaml. A relative Listen
// path is rejected -- unlike config.yaml's bridges:, there's no config
// file directory to sensibly resolve one against here. It returns the
// new bridge's info (not just its BridgeConfig) so a caller -- namely
// handleAdd -- doesn't need to fabricate a managedEntry of its own just
// to report Running/Source correctly.
func (m *bridgeManager) Add(cfg BridgeConfig) (bridgeInfo, error) {
	normalizeBridge(&cfg)
	if err := validateBridgeFields(cfg); err != nil {
		return bridgeInfo{}, badRequest("%s", err)
	}
	if !filepath.IsAbs(cfg.Listen) {
		return bridgeInfo{}, badRequest("bridge %q: listen path must be absolute when added through the management API (got %q)", cfg.Name, cfg.Listen)
	}
	if cfg.Listen == m.managementSocket {
		return bridgeInfo{}, conflictErr("listen path %q is the management socket itself", cfg.Listen)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, exists := m.entries[cfg.Name]; exists {
		if existing.rb == nil {
			return bridgeInfo{}, conflictErr("bridge %q already exists, stopped (%v) -- remove it first to replace it", cfg.Name, existing.lastError)
		}
		return bridgeInfo{}, conflictErr("bridge %q already exists", cfg.Name)
	}
	for _, e := range m.entries {
		if e.cfg.Listen == cfg.Listen {
			return bridgeInfo{}, conflictErr("listen path %q is already in use by bridge %q", cfg.Listen, e.cfg.Name)
		}
	}

	var entry *managedEntry
	if *cfg.Enabled {
		fatalCh := make(chan error, 1)
		rb, err := startBridge(m.ctx, m.dial, cfg, m.sockMode, m.group, fatalCh)
		if err != nil {
			return bridgeInfo{}, badRequest("starting bridge %q: %v", cfg.Name, err)
		}
		done := make(chan struct{})
		entry = &managedEntry{cfg: cfg, rb: rb, source: "managed", fatalDone: done}
		go m.watchFatal(entry, fatalCh, done)
	} else {
		// enabled: false in the request body -- register it, but don't
		// start it. Add's own listen-path-in-use check above still ran,
		// so this reserves the name/path the same as a running bridge
		// would, ready for a later Enable.
		entry = &managedEntry{cfg: cfg, source: "managed"}
	}
	m.entries[cfg.Name] = entry

	if err := m.saveLocked(); err != nil {
		// The bridge is live either way; a persistence failure means it
		// won't survive a restart, which is worth shouting about but not
		// worth tearing the bridge back down for.
		log.Printf("management: bridge %q started but failed to persist to %s: %v", cfg.Name, m.statePath, err)
	}

	return toBridgeInfo(entry), nil
}

// Remove stops name (if it's currently running -- a non-running entry,
// see managedEntry, has nothing to stop) and, if it was source
// "managed", drops it from managed-bridges.yaml. A bridge that came from
// config.yaml just stops running -- config.yaml isn't rewritten, so it
// returns on the next restart unless config.yaml is edited too.
//
// The entry is removed from the registry, and managed-bridges.yaml
// rewritten, before the (potentially slow, up to shutdownDrainTimeout)
// drain -- not after -- so Remove only holds m.mu long enough to update
// the registry, not for the whole drain. Otherwise every other request
// (GET /bridges, another Add or Remove) would block for however long
// this one bridge's in-flight connections take to finish, up to 10s.
func (m *bridgeManager) Remove(name string) error {
	m.mu.Lock()
	entry, ok := m.entries[name]
	if !ok {
		m.mu.Unlock()
		return notFoundErr("bridge %q not found", name)
	}
	delete(m.entries, name)
	if entry.fatalDone != nil {
		close(entry.fatalDone)
	}
	var saveErr error
	if entry.source == "managed" {
		saveErr = m.saveLocked()
	}
	m.mu.Unlock()

	if entry.rb != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownDrainTimeout)
		defer cancel()
		entry.rb.Shutdown(shutdownCtx)
	}
	if saveErr != nil {
		log.Printf("management: bridge %q removed but failed to update %s: %v", name, m.statePath, saveErr)
	}
	return nil
}

// Disable ensures name is not running, and remembers that as intentional
// (Enabled: false) -- unlike Remove, the bridge's config stays in the
// registry, so Enable can bring it back without resupplying
// listen/target/mode. Idempotent: a no-op, returning nil, if name is
// already disabled. If it was running, this is what actually makes its
// socket disappear -- see Remove's doc comment for why the registry is
// updated (and, for a "managed" bridge, persisted) before the drain,
// rather than holding m.mu for the whole thing.
//
// A "managed" bridge's disabled state is persisted (survives a
// restart); a "config"-sourced one is runtime-only, the same as Remove
// -- it comes back enabled on the next restart unless config.yaml's own
// enabled: is also set to false.
func (m *bridgeManager) Disable(name string) error {
	m.mu.Lock()
	entry, ok := m.entries[name]
	if !ok {
		m.mu.Unlock()
		return notFoundErr("bridge %q not found", name)
	}
	if entry.rb == nil && entry.cfg.Enabled != nil && !*entry.cfg.Enabled {
		m.mu.Unlock()
		return nil // already disabled
	}

	rb := entry.rb
	fatalDone := entry.fatalDone
	disabled := false
	entry.cfg.Enabled = &disabled
	entry.rb = nil
	entry.fatalDone = nil
	entry.lastError = nil // it's offline on purpose now, not because of whatever it last failed with
	if fatalDone != nil {
		close(fatalDone)
	}
	var saveErr error
	if entry.source == "managed" {
		saveErr = m.saveLocked()
	}
	m.mu.Unlock()

	if rb != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownDrainTimeout)
		defer cancel()
		rb.Shutdown(shutdownCtx)
	}
	if saveErr != nil {
		log.Printf("management: bridge %q disabled but failed to update %s: %v", name, m.statePath, saveErr)
	}
	return nil
}

// Enable ensures name is running: starting it if it's disabled, or
// retrying if it's enabled but currently down from a failure (Error
// explained why). Idempotent: a no-op, returning nil, if it's already
// running. Returns the same kind of error Add would if starting it now
// fails (its listen path has since become unavailable, e.g.) -- Enabled
// is left true either way, since the intent (it should be running) is
// unchanged; only Disable turns that intent back off.
func (m *bridgeManager) Enable(name string) error {
	m.mu.Lock()
	entry, ok := m.entries[name]
	if !ok {
		m.mu.Unlock()
		return notFoundErr("bridge %q not found", name)
	}
	if entry.rb != nil {
		m.mu.Unlock()
		return nil // already running
	}

	enabled := true
	entry.cfg.Enabled = &enabled
	fatalCh := make(chan error, 1)
	rb, err := startBridge(m.ctx, m.dial, entry.cfg, m.sockMode, m.group, fatalCh)
	if err != nil {
		entry.lastError = err
		m.mu.Unlock()
		return badRequest("starting bridge %q: %v", name, err)
	}
	done := make(chan struct{})
	entry.rb = rb
	entry.fatalDone = done
	entry.lastError = nil
	go m.watchFatal(entry, fatalCh, done)

	var saveErr error
	if entry.source == "managed" {
		saveErr = m.saveLocked()
	}
	m.mu.Unlock()

	if saveErr != nil {
		log.Printf("management: bridge %q enabled but failed to update %s: %v", name, m.statePath, saveErr)
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
	info := bridgeInfo{
		Name:        e.cfg.Name,
		Listen:      e.cfg.Listen,
		Target:      e.cfg.Target,
		Mode:        e.cfg.Mode,
		RewriteHost: e.cfg.RewriteHost,
		Source:      e.source,
		Enabled:     e.cfg.Enabled == nil || *e.cfg.Enabled,
		Running:     e.rb != nil,
	}
	if e.lastError != nil {
		info.Error = e.lastError.Error()
	}
	if hr, ok := e.rb.(healthReporter); ok {
		h := hr.dialHealth()
		info.DialFailures = h.ConsecutiveFailures
		info.LastDialError = h.LastError
		if !h.LastAttempt.IsZero() {
			t := h.LastAttempt
			info.LastDialAt = &t
		}
	}
	return info
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
		if e.rb == nil {
			continue // never started, or already dead -- nothing to shut down
		}
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
//	GET    /status                 -> the tailnet connection's own state
//	GET    /bridges                -> list every bridge tsbridge knows about
//	POST   /bridges                -> add a bridge (body: BridgeConfig JSON)
//	GET    /bridges/{name}         -> one bridge's info
//	DELETE /bridges/{name}         -> stop and forget one bridge
//	POST   /bridges/{name}/disable -> take one bridge offline, remembered
//	POST   /bridges/{name}/enable  -> bring one bridge back (or retry a failed one)
func (m *bridgeManager) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", m.handleStatus)
	mux.HandleFunc("GET /bridges", m.handleList)
	mux.HandleFunc("POST /bridges", m.handleAdd)
	mux.HandleFunc("GET /bridges/{name}", m.handleGet)
	mux.HandleFunc("DELETE /bridges/{name}", m.handleRemove)
	mux.HandleFunc("POST /bridges/{name}/disable", m.handleDisable)
	mux.HandleFunc("POST /bridges/{name}/enable", m.handleEnable)
	return mux
}

// statusInfo is GET /status's JSON body -- the subset of tsnet's own
// ipnstate.Status that's useful for confirming the tailnet connection
// itself (as opposed to any one bridge) is up.
type statusInfo struct {
	// BackendState is one of tsnet/tailscaled's own state names:
	// "NoState", "NeedsLogin", "NeedsMachineAuth", "Stopped", "Starting",
	// "Running". Bridges can only actually reach their targets when
	// this is "Running".
	BackendState string   `json:"backend_state"`
	TailscaleIPs []string `json:"tailscale_ips,omitempty"`
	Tailnet      string   `json:"tailnet,omitempty"`
	// Health lists active problems tailscaled itself has detected
	// (expired key, DNS misconfiguration, etc.) -- empty means none
	// known, not necessarily that everything is fine.
	Health []string `json:"health,omitempty"`
}

func (m *bridgeManager) handleStatus(w http.ResponseWriter, r *http.Request) {
	st, err := m.status(r.Context())
	if err != nil {
		writeError(w, fmt.Errorf("fetching tailnet status: %w", err))
		return
	}
	info := statusInfo{BackendState: st.BackendState, Health: st.Health}
	for _, ip := range st.TailscaleIPs {
		info.TailscaleIPs = append(info.TailscaleIPs, ip.String())
	}
	if st.CurrentTailnet != nil {
		info.Tailnet = st.CurrentTailnet.Name
	}
	writeJSON(w, http.StatusOK, info)
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
	writeJSON(w, http.StatusCreated, added)
}

func (m *bridgeManager) handleRemove(w http.ResponseWriter, r *http.Request) {
	if err := m.Remove(r.PathValue("name")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *bridgeManager) handleDisable(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := m.Disable(name); err != nil {
		writeError(w, err)
		return
	}
	m.writeBridge(w, name)
}

func (m *bridgeManager) handleEnable(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := m.Enable(name); err != nil {
		writeError(w, err)
		return
	}
	m.writeBridge(w, name)
}

// writeBridge writes name's current bridgeInfo as the response body, for
// handlers whose mutation (Disable/Enable) doesn't itself return one.
func (m *bridgeManager) writeBridge(w http.ResponseWriter, name string) {
	info, err := m.Get(name)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
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

	// httpBridge.Shutdown calls cancel() unconditionally, so this needs
	// its own cancellable ctx even though nothing here reads it except
	// the log-suppression check below -- see startHTTPBridge's identical
	// pattern in bridge.go.
	ctx, cancel := context.WithCancel(m.ctx)
	srv := &http.Server{
		Handler:           m.Handler(),
		ReadHeaderTimeout: httpReadHeaderTimeout,
	}

	go func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) && ctx.Err() == nil {
			log.Printf("management: server stopped: %v", err)
		}
	}()

	// health is never used (the management server doesn't implement
	// healthReporter's *bridge*-specific concern -- it doesn't dial a
	// Target at all), but httpBridge.dialHealth() dereferences it
	// unconditionally, so it still needs a non-nil value here.
	return &httpBridge{srv: srv, l: l, cancel: cancel, health: &dialHealth{}}, nil
}
