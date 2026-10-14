// manage.go implements the optional management API: a read-only HTTP
// server on its own Unix socket (management_socket: in config.yaml) that
// reports the tailnet connection's state and each bridge's health.
//
// It cannot change anything: bridges are defined by config.yaml alone and
// are added, changed or removed by editing it and restarting tsbridge.
//
// Every bridge lives in one bridgeManager registry. A bridge that fails
// to start, or later dies of a fatal error (see bridge.go's
// acceptLoop/http server error paths), stays in the registry as a
// non-running entry with the reason in Error, so GET /bridges can report
// it. One bridge dying does not take the rest of the process down (see
// main.go).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

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

func notFoundErr(format string, a ...any) error {
	return &apiError{http.StatusNotFound, fmt.Sprintf(format, a...)}
}

// bridgeInfo is the management API's JSON view of a bridge -- the
// BridgeConfig plus Running/Error and the bridge's dial health.
type bridgeInfo struct {
	Name        string  `json:"name"`
	Listen      string  `json:"listen"`
	Target      Targets `json:"target"`
	Mode        string  `json:"mode"`
	RewriteHost bool    `json:"rewrite_host"`
	// SocketGroup/SocketMode are the effective values: the bridge's own
	// override if set, else the global socket_group/socket_mode.
	// SocketGroup is empty when no group is applied.
	SocketGroup string `json:"socket_group"`
	SocketMode  string `json:"socket_mode"`
	// Enabled reflects operator intent, independent of whether the
	// bridge is currently Running: false means it was deliberately
	// taken offline (enabled: false in config.yaml) and has no socket. true with Running false instead means it's
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

// bridgeEntry is one bridge in bridgeManager's registry -- including a
// bridge that isn't currently running (rb == nil): it is disabled, failed
// to start, or started and later hit a fatal error. It stays in the
// registry either way so GET /bridges can report it and why it's down.
type bridgeEntry struct {
	cfg BridgeConfig
	rb  runningBridge // nil if not currently running
	// lastError explains why rb is nil, when that's due to a failure
	// (starting it, or a later fatal error) rather than never having
	// been attempted.
	lastError error
	// fatalDone is non-nil only while rb is non-nil: closed by Shutdown
	// when this entry is deliberately stopped, so its watchFatal
	// goroutine can stop waiting on a fatal error that will now never
	// come.
	fatalDone chan struct{}
}

// bridgeManager owns every bridge tsbridge runs, as started from
// config.yaml at boot.
type bridgeManager struct {
	ctx      context.Context
	dial     dialFunc
	sockMode os.FileMode
	group    string
	// status fetches the tailnet connection's own current state, for
	// GET /status. Set to a func wrapping srv.LocalClient().Status by
	// main.go; tests substitute a fake.
	status statusFunc

	mu      sync.Mutex
	entries map[string]*bridgeEntry
}

// statusFunc fetches the tailnet connection's current status --
// tsnet.Server.LocalClient().StatusWithoutPeers in production (see
// main.go), matched by *local.Client's own method signature so main.go
// can pass it in directly with no wrapping.
type statusFunc func(ctx context.Context) (*ipnstate.Status, error)

func newBridgeManager(ctx context.Context, dial dialFunc, sockMode os.FileMode, group string, status statusFunc) *bridgeManager {
	return &bridgeManager{
		ctx:      ctx,
		dial:     dial,
		sockMode: sockMode,
		group:    group,
		status:   status,
		entries:  make(map[string]*bridgeEntry),
	}
}

// startAll validates and starts bridges (config.yaml's list): a
// validation failure here is fatal to startup. A bridge that fails to
// *start* (a bad listen path, e.g.) is logged and skipped rather than
// failing startup, but stays in the registry as a non-running entry so
// it's visible via GET /bridges.
//
// It returns how many bridges ended up running against how many were
// attempted, so main can decide whether "zero running" is fatal.
func (m *bridgeManager) startAll(bridges []BridgeConfig) (started, attempted int, err error) {
	if err := checkBridges(bridges); err != nil {
		return 0, 0, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range bridges {
		s, a := m.startLocked(b)
		if s {
			started++
		}
		if a {
			attempted++
		}
	}
	return started, attempted, nil
}

// startLocked registers cfg, starting it unless it's disabled
// (enabled: false). Callers must hold m.mu. Reports outcome via return
// values rather than error -- a failed or disabled bridge is registered
// rather than treated as fatal -- distinguishing whether it was actually
// attempted, so startAll's caller can tell "nothing configured to run"
// from "tried and failed".
func (m *bridgeManager) startLocked(cfg BridgeConfig) (started, attempted bool) {
	if cfg.Enabled != nil && !*cfg.Enabled {
		m.entries[cfg.Name] = &bridgeEntry{cfg: cfg}
		return false, false
	}
	fatalCh := make(chan error, 1)
	rb, err := startBridge(m.ctx, m.dial, cfg, m.sockMode, m.group, fatalCh)
	if err != nil {
		log.Printf("bridge %s: failed to start, skipping: %v", cfg.Name, err)
		m.entries[cfg.Name] = &bridgeEntry{cfg: cfg, lastError: err}
		return false, true
	}
	done := make(chan struct{})
	entry := &bridgeEntry{cfg: cfg, rb: rb, fatalDone: done}
	m.entries[cfg.Name] = entry
	go m.watchFatal(entry, fatalCh, done)
	return true, true
}

// watchFatal marks entry as no longer running if it ever reports a fatal
// error -- clearing rb and recording err in lastError, but leaving the
// entry in the registry so it stays visible. It exits without doing
// anything if done closes first, which means this bridge was already
// stopped deliberately (process shutdown).
func (m *bridgeManager) watchFatal(entry *bridgeEntry, fatalCh <-chan error, done chan struct{}) {
	select {
	case err := <-fatalCh:
		m.mu.Lock()
		if cur, ok := m.entries[entry.cfg.Name]; ok && cur == entry && entry.fatalDone == done {
			entry.rb = nil
			entry.lastError = err
			entry.fatalDone = nil
			log.Printf("management: bridge %q stopped after a fatal error: %v", entry.cfg.Name, err)
		}
		m.mu.Unlock()
	case <-done:
	}
}

// Get returns the current info for one bridge.
func (m *bridgeManager) Get(name string) (bridgeInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[name]
	if !ok {
		return bridgeInfo{}, notFoundErr("bridge %q not found", name)
	}
	return m.toBridgeInfo(entry), nil
}

// List returns every bridge, running or not, sorted by name.
func (m *bridgeManager) List() []bridgeInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]bridgeInfo, 0, len(m.entries))
	for _, entry := range m.entries {
		out = append(out, m.toBridgeInfo(entry))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (m *bridgeManager) toBridgeInfo(e *bridgeEntry) bridgeInfo {
	// resolveSocketPerms can only fail on an unparseable socket_mode, which
	// validateBridgeFields already rejected, so a failed bridge never gets
	// here; report the configured value rather than substituting the global.
	mode, group, err := resolveSocketPerms(e.cfg, m.sockMode, m.group)
	modeStr := fmt.Sprintf("%04o", mode.Perm())
	if err != nil {
		modeStr = e.cfg.SocketMode
	}
	info := bridgeInfo{
		Name:        e.cfg.Name,
		Listen:      e.cfg.Listen,
		Target:      e.cfg.Targets,
		Mode:        e.cfg.Mode,
		RewriteHost: e.cfg.RewriteHost,
		SocketGroup: group,
		SocketMode:  modeStr,
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
	entries := make([]*bridgeEntry, 0, len(m.entries))
	for _, e := range m.entries {
		entries = append(entries, e)
	}
	m.entries = make(map[string]*bridgeEntry)
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, e := range entries {
		if e.rb == nil {
			continue // never started, or already dead -- nothing to shut down
		}
		wg.Add(1)
		go func(e *bridgeEntry) {
			defer wg.Done()
			e.rb.Shutdown(ctx)
			close(e.fatalDone)
		}(e)
	}
	wg.Wait()
}

// --- HTTP handlers ---

// Handler returns the management API's HTTP handler. It is read-only:
//
//	GET /status          -> the tailnet connection's own state
//	GET /bridges         -> list every bridge with its health
//	GET /bridges/{name}  -> one bridge's info
func (m *bridgeManager) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", m.handleStatus)
	mux.HandleFunc("GET /bridges", m.handleList)
	mux.HandleFunc("GET /bridges/{name}", m.handleGet)
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
