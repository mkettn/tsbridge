package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/user"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"tailscale.com/tsnet"
)

const (
	acceptBackoffMin = 5 * time.Millisecond
	acceptBackoffMax = 1 * time.Second
)

// startBridge creates the bridge's Unix socket (removing any stale one
// first) with the given permissions, then accepts connections in the
// background until ctx is cancelled. The returned listener's Close method
// unlinks the socket file.
//
// wg is used both for the accept loop goroutine and for each accepted
// connection's handler goroutine, so callers can wait for in-flight
// connections to finish before exiting. If the accept loop hits an error
// it can't recover from, it sends on fatal (non-blocking) so the caller
// can shut the whole process down rather than leaving a dead bridge
// silently bound but unserved.
func startBridge(ctx context.Context, srv *tsnet.Server, b ResolvedBridge, mode os.FileMode, group string, wg *sync.WaitGroup, fatal chan<- error) (net.Listener, error) {
	if err := removeStaleSocket(b.Listen); err != nil {
		return nil, err
	}

	l, err := listenWithMode(b.Listen, mode)
	if err != nil {
		return nil, fmt.Errorf("listening on unix socket: %w", err)
	}

	// listenWithMode already creates the socket with the right mode via
	// umask, but chmod again as cheap defense-in-depth (e.g. in case the
	// umask trick doesn't apply on some platform) -- this is a no-op in
	// the common case, not a new permissive window.
	if err := os.Chmod(b.Listen, mode); err != nil {
		l.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}

	if group != "" {
		gid, err := lookupGID(group)
		if err != nil {
			l.Close()
			return nil, fmt.Errorf("resolving socket_group %q: %w", group, err)
		}
		if err := os.Chown(b.Listen, -1, gid); err != nil {
			l.Close()
			return nil, fmt.Errorf("chown socket to group %q: %w", group, err)
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		acceptLoop(ctx, srv, b, l, wg, fatal)
	}()

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

// acceptLoop accepts connections on l until it's closed. Each connection
// is handled in its own goroutine so a slow or wedged peer, or an
// unreachable tailnet target, can't stall other bridges or other
// connections on this one.
//
// A transient error (e.g. hitting the process's open-file limit) is
// retried with backoff rather than ending the bridge. Any other error
// closes the listener and reports itself on fatal so the bridge doesn't
// keep the socket bound-but-dead with clients hanging in the backlog.
func acceptLoop(ctx context.Context, srv *tsnet.Server, b ResolvedBridge, l net.Listener, wg *sync.WaitGroup, fatal chan<- error) {
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
			handleConn(ctx, srv, b, conn)
		}()
	}
}

func isTemporaryAcceptError(err error) bool {
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) || errors.Is(err, syscall.ECONNABORTED)
}

var connCounter uint64

func handleConn(ctx context.Context, srv *tsnet.Server, b ResolvedBridge, local net.Conn) {
	defer local.Close()
	id := atomic.AddUint64(&connCounter, 1)

	remote, err := srv.Dial(ctx, "tcp", b.Target)
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
