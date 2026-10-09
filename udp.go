package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

const (
	// udpSessionIdleTimeout is how long a client's tailnet-side connection
	// is kept after its last datagram in either direction.
	udpSessionIdleTimeout = 60 * time.Second
	// udpMaxSessions bounds concurrent clients per bridge; datagrams from
	// new clients beyond it are dropped, so a flood of spoofed or distinct
	// source addresses can't exhaust file descriptors.
	udpMaxSessions = 1024
	udpMaxDatagram = 64 * 1024
)

// udpBridge is a udp:// listen: it relays datagrams between local clients
// and the tailnet target. UDP has no connections, so each distinct client
// address gets its own tailnet-side "connection" (a dial with network
// "udp"), which is how replies find their way back to the right client.
type udpBridge struct {
	name   string
	target string
	pc     net.PacketConn
	dial   dialFunc
	ctx    context.Context
	cancel context.CancelFunc
	health *dialHealth
	idle   time.Duration // set once at construction, never written again

	wg       sync.WaitGroup
	mu       sync.Mutex
	sessions map[string]*udpSession
}

type udpSession struct {
	remote net.Conn
	last   time.Time // guarded by udpBridge.mu
	// writeFailed is only touched by pump; it limits the reply-write
	// failure log to one line per session.
	writeFailed bool
}

func startUDPBridge(ctx context.Context, dial dialFunc, b BridgeConfig, addr string, idle time.Duration, fatal chan<- error) (runningBridge, error) {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("listening on udp %s: %w", addr, err)
	}
	warnIfNonLoopback(b.Name, pc.LocalAddr())

	ctx, cancel := context.WithCancel(ctx)
	health := &dialHealth{}
	u := &udpBridge{
		name: b.Name, target: b.Target, pc: pc, dial: health.wrap(dial),
		ctx: ctx, cancel: cancel, health: health, idle: idle,
		sessions: map[string]*udpSession{},
	}
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		u.readLoop(fatal)
	}()
	return u, nil
}

func (u *udpBridge) readLoop(fatal chan<- error) {
	buf := make([]byte, udpMaxDatagram)
	for {
		n, client, err := u.pc.ReadFrom(buf)
		if err != nil {
			if u.ctx.Err() != nil {
				return
			}
			log.Printf("bridge %s: fatal udp read error, stopping: %v", u.name, err)
			u.pc.Close()
			select {
			case fatal <- fmt.Errorf("bridge %s: udp read loop stopped: %w", u.name, err):
			default:
			}
			return
		}
		s := u.session(client)
		if s == nil {
			continue
		}
		if _, err := s.remote.Write(buf[:n]); err != nil {
			log.Printf("bridge %s: udp write to %s failed: %v", u.name, u.target, err)
		}
	}
}

// session returns the client's tailnet-side connection, dialing one on the
// first datagram. It must only be called from readLoop: the unlocked dial
// between the two critical sections below, and pump's deferred delete of its
// own key, are only safe because no second caller can add the same key
// concurrently. A nil result means the datagram is dropped (dial failed,
// or the session cap is reached) -- as with any UDP loss, the client's own
// retry logic is the recovery path.
func (u *udpBridge) session(client net.Addr) *udpSession {
	key := client.String()
	u.mu.Lock()
	if s, ok := u.sessions[key]; ok {
		s.last = time.Now()
		u.mu.Unlock()
		return s
	}
	full := len(u.sessions) >= udpMaxSessions
	u.mu.Unlock()
	if full {
		log.Printf("bridge %s: udp session limit (%d) reached, dropping datagram from %s", u.name, udpMaxSessions, key)
		return nil
	}

	remote, err := u.dial(u.ctx, "udp", u.target)
	if err != nil {
		log.Printf("bridge %s: udp dial %s failed: %v", u.name, u.target, err)
		return nil
	}
	s := &udpSession{remote: remote, last: time.Now()}
	u.mu.Lock()
	if u.ctx.Err() != nil { // shut down while dialing
		u.mu.Unlock()
		remote.Close()
		return nil
	}
	u.sessions[key] = s
	u.wg.Add(1)
	u.mu.Unlock()
	go func() {
		defer u.wg.Done()
		u.pump(key, client, s)
	}()
	return s
}

// pump copies replies from the tailnet target back to the client until the
// session goes idle or the bridge shuts down.
func (u *udpBridge) pump(key string, client net.Addr, s *udpSession) {
	defer func() {
		u.mu.Lock()
		delete(u.sessions, key)
		u.mu.Unlock()
		s.remote.Close()
	}()
	buf := make([]byte, udpMaxDatagram)
	for {
		s.remote.SetReadDeadline(time.Now().Add(u.idle))
		n, err := s.remote.Read(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() && u.ctx.Err() == nil {
				u.mu.Lock()
				active := time.Since(s.last) < u.idle // client sent something since
				u.mu.Unlock()
				if active {
					continue
				}
			}
			return
		}
		u.mu.Lock()
		s.last = time.Now()
		u.mu.Unlock()
		if _, err := u.pc.WriteTo(buf[:n], client); err != nil && !s.writeFailed {
			s.writeFailed = true
			log.Printf("bridge %s: udp reply to client %s failed (further failures for this session not logged): %v", u.name, client, err)
		}
	}
}

func (u *udpBridge) Shutdown(ctx context.Context) {
	u.cancel()
	u.pc.Close()
	u.mu.Lock()
	for _, s := range u.sessions {
		s.remote.Close() // unblocks each pump's Read
	}
	u.mu.Unlock()
	done := make(chan struct{})
	go func() {
		u.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (u *udpBridge) dialHealth() healthSnapshot { return u.health.snapshot() }
