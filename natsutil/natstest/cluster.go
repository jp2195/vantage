package natstest

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
)

// Cluster is three embedded nats-servers clustered with JetStream, for tests
// that need what only a cluster does: R3 streams, stream leaders, and
// leader elections.
type Cluster struct {
	Servers []*server.Server
}

// routePoolSize is the number of pooled route connections each server opens
// to each other server for client accounts, set explicitly so ready can
// count them. It is nats-server's default.
const routePoolSize = 3

// RunCluster starts a three-server JetStream cluster on loopback and waits
// until it can place an R3 stream and answer for it (see ready). The servers are shut down via
// t.Cleanup.
//
// Cluster ports are chosen by listening on port 0 and closing, so another
// process can take one before its server binds it; the server then never
// becomes ready. A start that fails that way is torn down and retried on
// fresh ports.
func RunCluster(t *testing.T) *Cluster {
	t.Helper()
	const starts = 3
	var err error
	for range starts {
		var c *Cluster
		if c, err = startCluster(t); err == nil {
			t.Cleanup(c.shutdown)
			return c
		}
	}
	t.Fatalf("cluster did not start in %d tries: %v", starts, err)
	return nil
}

func startCluster(t *testing.T) (*Cluster, error) {
	const n = 3
	ports := make([]int, n)
	routes := make([]string, n)
	for i := range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		ports[i] = l.Addr().(*net.TCPAddr).Port
		l.Close()
		routes[i] = fmt.Sprintf("nats://127.0.0.1:%d", ports[i])
	}
	c := &Cluster{}
	for i := range n {
		o := &server.Options{
			ServerName: fmt.Sprintf("n%d", i), Host: "127.0.0.1", Port: -1,
			JetStream: true, StoreDir: t.TempDir(),
			Cluster: server.ClusterOpts{Name: "natstest", Host: "127.0.0.1", Port: ports[i],
				PoolSize: routePoolSize},
			Routes: server.RoutesFromStr(strings.Join(routes, ",")),
			// Short, so a lame-duck shutdown in a test takes seconds.
			LameDuckDuration: 2 * time.Second, LameDuckGracePeriod: 500 * time.Millisecond,
		}
		s, err := server.NewServer(o)
		if err != nil {
			c.shutdown()
			return nil, err
		}
		go s.Start()
		c.Servers = append(c.Servers, s)
	}
	for _, s := range c.Servers {
		if !s.ReadyForConnections(10 * time.Second) {
			c.shutdown()
			return nil, fmt.Errorf("server %s not ready", s.Name())
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for !c.ready() {
		if time.Now().After(deadline) {
			c.shutdown()
			return nil, fmt.Errorf("no JetStream meta leader with all peers current after 30s")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return c, nil
}

func (c *Cluster) shutdown() {
	for _, s := range c.Servers {
		s.Shutdown()
	}
}

// ready reports whether the cluster is ready to place an R3 stream and
// answer for it: one server leads the JetStream meta group and has current
// stats from all three peers (JetStreamClusterPeers, which only the leader
// answers), every server is current with it, and every route between every
// pair of servers is up (routesUp). A meta leader alone is not enough;
// stream creation then intermittently waits out its context.
func (c *Cluster) ready() bool {
	if !c.routesUp() {
		return false
	}
	leader := false
	for _, s := range c.Servers {
		if !s.JetStreamIsCurrent() {
			return false
		}
		if s.JetStreamIsLeader() {
			leader = len(s.JetStreamClusterPeers()) == len(c.Servers)
		}
	}
	return leader
}

// routesUp reports whether every server has all its route connections to
// every other server: the system account's dedicated route, and
// routePoolSize pooled routes for client accounts.
//
// JetStream's meta group runs over the system account's route, so it can
// elect a leader and report every peer current while the pooled routes that
// carry client traffic are still connecting. A stream created then is
// created, and its leader sends the create response to the client's reply
// inbox; if the pooled route between the leader's server and the client's
// server is not up yet, the leader does not know the inbox exists and drops
// the response, and the client waits out its context. Measured under a
// parallel `make test`: every such timeout started with a missing pooled
// route from the client's server.
func (c *Cluster) routesUp() bool {
	for _, s := range c.Servers {
		rz, err := s.Routez(nil)
		if err != nil {
			return false
		}
		pooled, system := map[string]int{}, map[string]int{}
		for _, r := range rz.Routes {
			if r.Account == "" {
				pooled[r.RemoteName]++
			} else {
				system[r.RemoteName]++
			}
		}
		for _, o := range c.Servers {
			if o == s {
				continue
			}
			if pooled[o.Name()] != routePoolSize || system[o.Name()] != 1 {
				return false
			}
		}
	}
	return true
}

// StreamLeader waits for, and returns, the server leading stream in the
// global account.
func (c *Cluster) StreamLeader(t *testing.T, stream string) *server.Server {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		for _, s := range c.Servers {
			if s.Running() && s.JetStreamIsStreamLeader(server.DEFAULT_GLOBAL_ACCOUNT, stream) {
				return s
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream %s has no leader after 30s", stream)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Other returns a running server that is not s.
func (c *Cluster) Other(s *server.Server) *server.Server {
	for _, o := range c.Servers {
		if o != s && o.Running() {
			return o
		}
	}
	return nil
}
