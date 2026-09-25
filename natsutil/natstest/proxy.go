package natstest

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
)

// RunServer starts an embedded nats-server with JetStream enabled on a random
// port and a temporary store directory, shut down via t.Cleanup. Use it when
// a test needs the server itself -- to put a Proxy in front of it, say --
// rather than a connection to it.
func RunServer(t *testing.T) *server.Server {
	t.Helper()
	opts := &server.Options{Port: -1, JetStream: true, StoreDir: t.TempDir()}
	srv, err := server.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)
	return srv
}

// Proxy is a TCP relay between NATS clients and one upstream server that a
// test can hold and cut, to put the client connection into states a healthy
// loopback server never produces on demand.
//
// The one it exists for is a publish the server has stored but whose ack the
// client never sees: FreezeToClient holds every byte the server sends, the
// PubAck included, and Sever then drops the connection with those bytes
// still held. That is the lame-duck restart case, reproduced deterministically
// instead of by racing a server shutdown against a publish. FreezeToServer is
// the other half: a publish the client wrote that never reaches the server at
// all.
//
// Held bytes are released in order when the direction is thawed, or discarded
// when the connection is severed. Freezing is proxy-wide and applies to
// connections opened later too, so a frozen proxy also stalls a reconnect's
// handshake: thaw before expecting the client to come back.
type Proxy struct {
	ln       net.Listener
	upstream string

	mu       sync.Mutex
	cond     *sync.Cond
	toServer bool // frozen client->server
	toClient bool // frozen server->client
	refuse   bool
	conns    map[net.Conn]struct{}
	closed   bool
}

// NewProxy listens on a random loopback port and relays each connection to
// upstream (host:port). It is closed via t.Cleanup.
func NewProxy(t *testing.T, upstream string) *Proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{ln: ln, upstream: upstream, conns: map[net.Conn]struct{}{}}
	p.cond = sync.NewCond(&p.mu)
	go p.accept()
	t.Cleanup(p.Close)
	return p
}

// URL is the nats:// URL clients should connect to.
func (p *Proxy) URL() string { return "nats://" + p.ln.Addr().String() }

// FreezeToClient holds everything the server sends until ThawToClient or
// Sever.
func (p *Proxy) FreezeToClient() { p.set(func() { p.toClient = true }) }

// ThawToClient releases what FreezeToClient held.
func (p *Proxy) ThawToClient() { p.set(func() { p.toClient = false }) }

// FreezeToServer holds everything clients send until ThawToServer or Sever.
func (p *Proxy) FreezeToServer() { p.set(func() { p.toServer = true }) }

// ThawToServer releases what FreezeToServer held.
func (p *Proxy) ThawToServer() { p.set(func() { p.toServer = false }) }

// Refuse makes the proxy close every new connection at once, so a client
// that loses its connection cannot get it back. Allow undoes it.
func (p *Proxy) Refuse() { p.set(func() { p.refuse = true }) }

// Allow undoes Refuse.
func (p *Proxy) Allow() { p.set(func() { p.refuse = false }) }

// Sever closes every open connection, discarding whatever is held in either
// direction, and thaws both directions so a reconnect can complete.
func (p *Proxy) Sever() {
	p.mu.Lock()
	for c := range p.conns {
		c.Close()
	}
	p.conns = map[net.Conn]struct{}{}
	p.toServer, p.toClient = false, false
	p.cond.Broadcast()
	p.mu.Unlock()
}

// Close stops the proxy and severs every connection.
func (p *Proxy) Close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.ln.Close()
	p.Sever()
}

func (p *Proxy) set(f func()) {
	p.mu.Lock()
	f()
	p.cond.Broadcast()
	p.mu.Unlock()
}

func (p *Proxy) accept() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		refuse := p.refuse || p.closed
		p.mu.Unlock()
		if refuse {
			c.Close()
			continue
		}
		u, err := net.Dial("tcp", p.upstream)
		if err != nil {
			c.Close()
			continue
		}
		p.mu.Lock()
		p.conns[c] = struct{}{}
		p.conns[u] = struct{}{}
		p.mu.Unlock()
		go p.relay(u, c, func() bool { return p.toServer })
		go p.relay(c, u, func() bool { return p.toClient })
	}
}

// relay copies src to dst, holding each chunk read while frozen() is true.
// A chunk held when the connection is severed is never written.
func (p *Proxy) relay(dst, src net.Conn, frozen func() bool) {
	defer dst.Close()
	defer src.Close()
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if !p.waitThawed(src, frozen) {
				return
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// waitThawed blocks while frozen() and reports whether conn is still open.
func (p *Proxy) waitThawed(conn net.Conn, frozen func() bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for frozen() {
		if _, open := p.conns[conn]; !open {
			return false
		}
		p.cond.Wait()
	}
	_, open := p.conns[conn]
	return open
}
