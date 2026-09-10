package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/user"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	acceptBackoffMin = 5 * time.Millisecond
	acceptBackoffMax = 1 * time.Second

	// httpReadHeaderTimeout bounds how long an http-mode bridge waits for
	// a client to finish sending request headers, so a slow/idle client
	// on the Unix socket can't tie up a handler indefinitely.
	httpReadHeaderTimeout = 10 * time.Second
)

// dialFunc opens a connection to the tailnet target. In production this
// is always srv.Dial (tsnet.Server's method value matches this type
// exactly); tests substitute a fake to exercise bridge logic without a
// real tailnet.
type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// runningBridge is what startBridge hands back: something main can stop
// uniformly regardless of mode. Shutdown stops the bridge from accepting
// new work and waits, bounded by ctx, for whatever's in flight to finish
// before returning; the Unix socket file is unlinked either way.
type runningBridge interface {
	Shutdown(ctx context.Context)
}

// startBridge creates the bridge's Unix socket (removing any stale one
// first) with the given permissions, then starts serving it in the
// background according to b.Mode until Shutdown is called on the
// returned runningBridge.
//
// If serving hits an error it can't recover from, it sends on fatal
// (non-blocking) so the caller can shut the whole process down rather
// than leaving a dead bridge silently bound but unserved.
func startBridge(ctx context.Context, dial dialFunc, b BridgeConfig, sockMode os.FileMode, group string, fatal chan<- error) (runningBridge, error) {
	l, err := createUnixSocket(b.Listen, sockMode, group)
	if err != nil {
		return nil, err
	}

	switch b.Mode {
	case "http":
		return startHTTPBridge(ctx, dial, b, l, fatal), nil
	default: // "tcp", the only other value checkBridges allows
		return startTCPBridge(ctx, dial, b, l, fatal), nil
	}
}

// createUnixSocket creates a Unix socket at path (removing any stale one
// first) with the given permissions and, if group is non-empty, group
// ownership. Shared by startBridge and the management socket (manage.go),
// which need identical stale-socket handling and permission setup for
// their own listener.
func createUnixSocket(path string, mode os.FileMode, group string) (net.Listener, error) {
	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}

	l, err := listenWithMode(path, mode)
	if err != nil {
		return nil, fmt.Errorf("listening on unix socket: %w", err)
	}

	// listenWithMode already creates the socket with the right mode via
	// umask, but chmod again as cheap defense-in-depth (e.g. in case the
	// umask trick doesn't apply on some platform) -- this is a no-op in
	// the common case, not a new permissive window.
	if err := os.Chmod(path, mode); err != nil {
		l.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}

	if group != "" {
		gid, err := lookupGID(group)
		if err != nil {
			l.Close()
			return nil, fmt.Errorf("resolving socket_group %q: %w", group, err)
		}
		if err := os.Chown(path, -1, gid); err != nil {
			l.Close()
			return nil, fmt.Errorf("chown socket to group %q: %w", group, err)
		}
	}

	return l, nil
}

// removeStaleSocket removes b.Listen only if it's genuinely a leftover
// socket from an unclean shutdown: it refuses to touch a path that isn't
// a socket at all (a typo'd listen: path pointing at a real file), and
// refuses to steal a socket that another running instance still has
// bound and accepting.
func removeStaleSocket(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to remove %s: not a socket", path)
	}
	if c, err := net.Dial("unix", path); err == nil {
		c.Close()
		return fmt.Errorf("%s is in use by another instance", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("removing stale socket: %w", err)
	}
	return nil
}

var listenMu sync.Mutex

// listenWithMode binds a Unix socket that never has a mode wider than
// mode, even momentarily: net.Listen's bind(2) applies the process umask
// to the socket file it creates, so narrowing the umask for the
// duration of the call closes the window where a freshly created socket
// would otherwise sit at the default (e.g. 0755) until a later chmod.
// syscall.Umask is process-global, hence the mutex -- which only
// serializes our own calls here. By the time bridges start, srv.Up has
// already returned and tsnet's background goroutines are running
// without holding listenMu, so anything *they* create during this
// narrow window inherits the tightened umask too. Umask only clears
// bits, never sets them, so nothing can come out wider than intended;
// the residual risk runs the other way (something coming out narrower,
// e.g. losing an execute bit on a directory) and is very unlikely in
// practice since the window is a single bind(2) call.
func listenWithMode(path string, mode os.FileMode) (net.Listener, error) {
	listenMu.Lock()
	defer listenMu.Unlock()
	old := syscall.Umask(0777 &^ int(mode))
	defer syscall.Umask(old)
	return net.Listen("unix", path)
}

func lookupGID(name string) (int, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(g.Gid)
}

// tcpBridge is mode: tcp -- a raw bidirectional byte copy between the
// Unix socket and the tailnet target, one goroutine tree per accepted
// connection.
type tcpBridge struct {
	l      net.Listener
	cancel context.CancelFunc // cancels this bridge's own ctx; see Shutdown
	wg     sync.WaitGroup     // acceptLoop + one handleConn goroutine per connection
}

func startTCPBridge(ctx context.Context, dial dialFunc, b BridgeConfig, l net.Listener, fatal chan<- error) *tcpBridge {
	ctx, cancel := context.WithCancel(ctx)
	t := &tcpBridge{l: l, cancel: cancel}
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		acceptLoop(ctx, dial, b, l, &t.wg, fatal)
	}()
	return t
}

// Shutdown cancels this bridge's own context first, so acceptLoop's
// ctx.Err() check recognizes the listener close below as deliberate
// rather than reporting it as a fatal error -- this matters beyond
// process-wide shutdown: the management API (manage.go) calls Shutdown
// on one bridge at a time while the process, and every other bridge,
// keeps running, and a deliberate removal must not look like a crash.
// It then closes the listener (unlinking the socket) and waits, bounded
// by ctx, for the accept loop and any in-flight connections to finish.
func (t *tcpBridge) Shutdown(ctx context.Context) {
	t.cancel()
	t.l.Close()
	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// acceptLoop accepts connections on l until it's closed. Each connection
// is handled in its own goroutine so a slow or wedged peer, or an
// unreachable tailnet target, can't stall other bridges or other
// connections on this one.
//
// A transient error (e.g. hitting the process's open-file limit) is
// retried with backoff rather than ending the bridge. Any other error
// closes the listener and reports itself on fatal so the bridge doesn't
// keep the socket bound-but-dead with clients hanging in the backlog.
func acceptLoop(ctx context.Context, dial dialFunc, b BridgeConfig, l net.Listener, wg *sync.WaitGroup, fatal chan<- error) {
	backoff := acceptBackoffMin
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return // shutting down; listener was closed intentionally
			}
			if isTemporaryAcceptError(err) {
				log.Printf("bridge %s: temporary accept error, retrying in %v: %v", b.Name, backoff, err)
				time.Sleep(backoff)
				if backoff *= 2; backoff > acceptBackoffMax {
					backoff = acceptBackoffMax
				}
				continue
			}
			log.Printf("bridge %s: fatal accept error, stopping: %v", b.Name, err)
			l.Close()
			select {
			case fatal <- fmt.Errorf("bridge %s: accept loop stopped: %w", b.Name, err):
			default:
			}
			return
		}
		backoff = acceptBackoffMin
		wg.Add(1)
		go func() {
			defer wg.Done()
			handleConn(ctx, dial, b, conn)
		}()
	}
}

func isTemporaryAcceptError(err error) bool {
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) || errors.Is(err, syscall.ECONNABORTED)
}

var connCounter uint64

func handleConn(ctx context.Context, dial dialFunc, b BridgeConfig, local net.Conn) {
	defer local.Close()
	id := atomic.AddUint64(&connCounter, 1)

	remote, err := dial(ctx, "tcp", b.Target)
	if err != nil {
		log.Printf("bridge %s: conn %d: dial %s failed: %v", b.Name, id, b.Target, err)
		return
	}
	defer remote.Close()

	log.Printf("bridge %s: conn %d: opened (%s -> %s)", b.Name, id, b.Listen, b.Target)

	var copyWG sync.WaitGroup
	copyWG.Add(2)
	go func() {
		defer copyWG.Done()
		io.Copy(remote, local)
		closeWrite(remote)
	}()
	go func() {
		defer copyWG.Done()
		io.Copy(local, remote)
		closeWrite(local)
	}()
	copyWG.Wait()

	log.Printf("bridge %s: conn %d: closed", b.Name, id)
}

// closeWrite half-closes the write side of c, if supported, so the peer
// sees EOF on its read instead of the copy in the other direction
// blocking indefinitely.
func closeWrite(c net.Conn) {
	type writeCloser interface {
		CloseWrite() error
	}
	if wc, ok := c.(writeCloser); ok {
		wc.CloseWrite()
	}
}

// httpBridge is mode: http -- an HTTP server on the Unix socket that
// terminates each request and reverse-proxies it to the tailnet target,
// dialing out through dial rather than raw-copying bytes.
type httpBridge struct {
	srv    *http.Server
	l      net.Listener
	cancel context.CancelFunc // cancels this bridge's own ctx; see Shutdown
}

func startHTTPBridge(ctx context.Context, dial dialFunc, b BridgeConfig, l net.Listener, fatal chan<- error) *httpBridge {
	ctx, cancel := context.WithCancel(ctx)
	targetURL := &url.URL{Scheme: "http", Host: b.Target}
	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	// NewSingleHostReverseProxy's default Director rewrites r.URL.Host
	// (what gets dialed) but leaves r.Host -- the actual Host header
	// sent on the wire -- as whatever arrived on the incoming request.
	// That's the default here too (b.RewriteHost false): most backends
	// don't care what Host they're addressed as. A virtual-host-style
	// one does (tailscale serve, for one: it keys routes by the target
	// node's own hostname, and 404s on anything else) -- rewrite_host:
	// true forces r.Host to target's host so a backend like that
	// recognizes the request as its own.
	if b.RewriteHost {
		defaultDirector := proxy.Director
		proxy.Director = func(r *http.Request) {
			defaultDirector(r)
			r.Host = targetURL.Host
		}
	}
	proxy.Transport = &http.Transport{
		DialContext: func(dialCtx context.Context, network, addr string) (net.Conn, error) {
			return dial(dialCtx, "tcp", addr)
		},
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		log.Printf("bridge %s: %s %s -> %s: %d", b.Name, resp.Request.Method, resp.Request.URL.RequestURI(), b.Target, resp.StatusCode)
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("bridge %s: %s %s -> %s: %v", b.Name, r.Method, r.URL.RequestURI(), b.Target, err)
		w.WriteHeader(http.StatusBadGateway)
	}

	httpSrv := &http.Server{
		Handler:           proxy,
		ReadHeaderTimeout: httpReadHeaderTimeout,
	}

	go func() {
		err := httpSrv.Serve(l)
		if err != nil && !errors.Is(err, http.ErrServerClosed) && ctx.Err() == nil {
			log.Printf("bridge %s: fatal http server error, stopping: %v", b.Name, err)
			select {
			case fatal <- fmt.Errorf("bridge %s: http server stopped: %w", b.Name, err):
			default:
			}
		}
	}()

	return &httpBridge{srv: httpSrv, l: l, cancel: cancel}
}

// Shutdown stops accepting new connections and waits, bounded by ctx,
// for in-flight requests to finish; if ctx runs out first, it force-closes
// whatever's left rather than leaving it to linger past shutdown.
//
// It closes l itself rather than relying solely on http.Server.Shutdown
// to do it: Shutdown only closes listeners Serve has already registered
// (via trackListener), so a Shutdown call racing the Serve goroutine's
// own startup can find nothing tracked yet and return nil having closed
// nothing -- which would also skip the Close() fallback below, since
// that only fires on a non-nil error. Closing l unconditionally here
// means the socket file is unlinked regardless of that race; Serve
// wraps l in a once-close wrapper and net.UnixListener.Close is safe to
// call twice, so closing it again from Serve's own deferred cleanup is
// harmless.
//
// cancel is called first so the Serve goroutine's ctx.Err() check (used
// to decide whether a non-ErrServerClosed Serve error is worth logging)
// recognizes this as deliberate. Serve returning exactly ErrServerClosed
// already covers the common case on its own, but cancelling here keeps
// both bridge types symmetric and covers startManagementServer, which
// builds an httpBridge directly rather than through startHTTPBridge.
func (h *httpBridge) Shutdown(ctx context.Context) {
	h.cancel()
	defer h.l.Close()
	if err := h.srv.Shutdown(ctx); err != nil {
		h.srv.Close()
	}
}
