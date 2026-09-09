package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/user"
	"strconv"
	"sync"
	"sync/atomic"

	"tailscale.com/tsnet"
)

// startBridge creates the bridge's Unix socket (removing any stale one
// first) with the given permissions, then accepts connections in the
// background until ctx is cancelled. The returned listener's Close method
// unlinks the socket file.
func startBridge(ctx context.Context, srv *tsnet.Server, b ResolvedBridge, mode os.FileMode, group string, wg *sync.WaitGroup) (net.Listener, error) {
	if err := os.Remove(b.Listen); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("removing stale socket: %w", err)
	}

	l, err := net.Listen("unix", b.Listen)
	if err != nil {
		return nil, fmt.Errorf("listening on unix socket: %w", err)
	}

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
		acceptLoop(ctx, srv, b, l)
	}()

	return l, nil
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
func acceptLoop(ctx context.Context, srv *tsnet.Server, b ResolvedBridge, l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return // shutting down; listener was closed intentionally
			}
			log.Printf("bridge %s: accept error, stopping: %v", b.Name, err)
			return
		}
		go handleConn(ctx, srv, b, conn)
	}
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

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(remote, local)
		closeWrite(remote)
	}()
	go func() {
		defer wg.Done()
		io.Copy(local, remote)
		closeWrite(local)
	}()
	wg.Wait()

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
