// Package uplink is a pod's or client's connection to a fibhub server: a
// pool of arpc clients that keep dialing until they connect, and register
// each time they connect. The uplink pings the server to measure the link,
// over the registered client the server numbered lowest. Each tunnel goes
// over one client of the pool throughout, so its frames keep their order,
// and closes when that client loses the server.
package uplink

import (
	"errors"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib/arpc"
	"github.com/lesismal/fibhub/internal/bodycrypt"
	"github.com/lesismal/fibhub/internal/latency"
	"github.com/lesismal/fibhub/internal/proto"
	"github.com/lesismal/fibhub/internal/tunnel"
	"github.com/lesismal/fibhub/internal/wsarpc"
)

// Timeouts and intervals of an uplink.
var (
	DialTimeout   = 5 * time.Second
	CallTimeout   = 10 * time.Second
	RetryInterval = 3 * time.Second
	// PingInterval is how often a registered uplink pings the server, and
	// PingTimeout how long it waits for the answer before it counts the
	// ping lost.
	PingInterval = 5 * time.Second
	PingTimeout  = 5 * time.Second
)

// DefaultPoolSize is how many connections to a server an uplink makes when
// its Config does not say.
const DefaultPoolSize = 10

// Config is a server to connect to.
type Config struct {
	// Name labels the server locally; it defaults to Addr.
	Name string `json:"name"`
	// Addr is the server's arpc address, host:port, or the URL of its arpc
	// WebSocket, ws://host:port/path.
	Addr     string `json:"addr"`
	Password string `json:"password"`
	// BodyPassword, when set, has every arpc message's body encrypted with
	// AES under a key derived from it, as the server must be configured to.
	BodyPassword string `json:"bodyPassword,omitempty"`
	// PoolSize is how many connections to the server carry tunnels; it
	// defaults to DefaultPoolSize.
	PoolSize int `json:"poolSize,omitempty"`
}

func (cfg *Config) poolSize() int {
	if cfg.PoolSize <= 0 {
		return DefaultPoolSize
	}
	return cfg.PoolSize
}

// The states of an uplink.
const (
	StateConnecting = "connecting"
	StateRegistered = "registered"
	StateFailed     = "failed"
	StateStopped    = "stopped"
)

// Status is an uplink as a status page shows it.
type Status struct {
	Name    string    `json:"name"`
	Addr    string    `json:"addr"`
	State   string    `json:"state"`
	Error   string    `json:"error,omitempty"`
	Server  string    `json:"server,omitempty"`
	PeerID  uint32    `json:"peerId,omitempty"`
	Since   time.Time `json:"since,omitempty"`
	Tunnels int       `json:"tunnels"`
	// RTT is the round trip to the server, as the pings measure it.
	RTT latency.Stats `json:"rtt"`
	// BodyEncrypted reports whether arpc bodies are encrypted.
	BodyEncrypted bool `json:"bodyEncrypted"`
	// PoolSize is how many connections the uplink makes, Pool how many of
	// them are registered now, and Places the places the server gave those
	// among the connections of the peer's name, lowest first.
	PoolSize int   `json:"poolSize"`
	Pool     int   `json:"pool"`
	Places   []int `json:"places"`
}

// Uplink is the connection to one server.
type Uplink struct {
	Config  Config
	Tunnels *tunnel.Table
	// OnRegistered, if set, runs each time the uplink registers its first
	// connection, after it had none.
	OnRegistered func(*Uplink)
	// OnPing, if set, adds to each ping what else the peer measured.
	OnPing func(*proto.PingReq)
	// OnPong, if set, gets the server's answer to each ping.
	OnPong func(*Uplink, *proto.PingRsp)

	engine  *fib.Engine
	info    func() proto.Info
	handler *arpc.Handler
	seq     atomic.Uint32
	rtt     latency.Meter
	pinging atomic.Bool

	mu sync.Mutex
	// pool is every client dialed, and conns those registered, with what
	// their registrations were answered with.
	pool    []*arpc.Client
	conns   map[*arpc.Client]proto.RegisterRsp
	state   string
	lastErr string
	server  string
	since   time.Time
	stopped bool
	stop    chan struct{}
}

// New is an uplink to the server cfg names, which registers as what info
// returns. setup registers the methods the server may call on it.
func New(engine *fib.Engine, cfg Config, info func() proto.Info, setup func(u *Uplink, h *arpc.Handler)) *Uplink {
	if cfg.Name == "" {
		cfg.Name = cfg.Addr
	}
	u := &Uplink{Config: cfg, Tunnels: tunnel.NewTable(), engine: engine, info: info, conns: map[*arpc.Client]proto.RegisterRsp{}, state: StateConnecting, stop: make(chan struct{})}
	h := arpc.NewHandler()
	h.SetReconnectInterval(RetryInterval)
	if cfg.BodyPassword != "" {
		coder, err := bodycrypt.New(cfg.BodyPassword)
		if err != nil {
			u.state, u.lastErr = StateFailed, err.Error()
		} else {
			h.UseCoder(coder)
		}
	}
	u.Tunnels.Register(h)
	h.Handle(proto.MethodKick, func(ctx *arpc.Context) {
		u.fail("kicked by the server: "+string(ctx.Body()), true)
	}, false)
	if setup != nil {
		setup(u, h)
	}
	h.HandleConnected(func(c *arpc.Client) { go u.register(c) })
	h.HandleReconnect(func(c *arpc.Client, info *arpc.ReconnectInfo) {
		if info.Times == 1 {
			u.lost(c)
		}
		if !info.Success && info.Err != nil {
			u.fail(info.Err.Error(), false)
		}
	})
	u.handler = h
	return u
}

// Start dials the pool's connections to the server, and goes on dialing
// each until it connects or the uplink stops; the arpc clients reconnect on
// their own from then on.
func (u *Uplink) Start() {
	u.mu.Lock()
	failed := u.state == StateFailed
	u.mu.Unlock()
	if failed {
		// The config is wrong, which dialing again does not fix.
		return
	}
	dialer := arpc.TCPDialer(u.engine, "tcp", u.Config.Addr, DialTimeout)
	if wsarpc.IsURL(u.Config.Addr) {
		d, err := wsarpc.Dialer(u.engine, u.Config.Addr, DialTimeout)
		if err != nil {
			u.fail(err.Error(), false)
			return
		}
		dialer = d
	}
	go u.pingLoop()
	for range u.Config.poolSize() {
		go u.dial(dialer)
	}
}

// dial dials a client of the pool, and goes on dialing until it connects or
// the uplink stops.
func (u *Uplink) dial(dialer arpc.DialerFunc) {
	for {
		c, err := arpc.NewClient(dialer, u.handler)
		u.mu.Lock()
		if u.stopped {
			u.mu.Unlock()
			if c != nil {
				c.Stop()
			}
			return
		}
		if err == nil {
			u.pool = append(u.pool, c)
			u.mu.Unlock()
			return
		}
		u.mu.Unlock()
		u.fail(err.Error(), false)
		select {
		case <-u.stop:
			return
		case <-time.After(RetryInterval):
		}
	}
}

// register authenticates and registers on c, which has just connected.
func (u *Uplink) register(c *arpc.Client) {
	conn := c.Conn()
	if conn == nil {
		return
	}
	// Nothing the connection carried before goes on.
	u.Tunnels.CloseLink(c, "reconnected")
	req := proto.RegisterReq{Info: u.info()}
	proto.Sign(&req, u.Config.Password)
	var rsp proto.RegisterRsp
	if err := c.Call(proto.MethodRegister, &req, &rsp, CallTimeout); err != nil {
		u.fail("register: "+err.Error(), false)
		slog.Warn("fibhub: register failed", "server", u.Config.Addr, "err", err)
		// The arpc client dials again once the connection closes, after
		// an interval, rather than at once.
		time.AfterFunc(RetryInterval, func() { _ = conn.Close() })
		return
	}
	u.mu.Lock()
	if u.stopped || c.Conn() != conn {
		u.mu.Unlock()
		return
	}
	first := len(u.conns) == 0
	u.conns[c] = rsp
	u.state, u.lastErr, u.server = StateRegistered, "", rsp.Server
	if first {
		u.since = time.Now()
	}
	f := u.OnRegistered
	u.mu.Unlock()
	slog.Info("fibhub: registered", "server", u.Config.Addr, "name", rsp.Server, "id", rsp.Peer, "index", rsp.Index, "conn", rsp.Conn)
	if !first {
		return
	}
	u.rtt.Reset()
	if f != nil {
		f(u)
	}
	// Measure the link at once, rather than an interval from now.
	go u.ping()
}

// lost records that c lost its connection, and closes the tunnels it
// carried.
func (u *Uplink) lost(c *arpc.Client) {
	u.mu.Lock()
	delete(u.conns, c)
	none := len(u.conns) == 0
	if none && u.state == StateRegistered {
		u.state = StateConnecting
	}
	u.mu.Unlock()
	if none {
		u.rtt.Reset()
	}
	u.Tunnels.CloseLink(c, "server connection lost")
}

// fail records why the uplink is not registered: always, or only while
// none of its connections is.
func (u *Uplink) fail(msg string, always bool) {
	u.mu.Lock()
	if !u.stopped && (always || len(u.conns) == 0) {
		u.state, u.lastErr = StateFailed, msg
	}
	u.mu.Unlock()
}

// pingLoop pings the server every PingInterval until the uplink stops.
func (u *Uplink) pingLoop() {
	t := time.NewTicker(PingInterval)
	defer t.Stop()
	for {
		select {
		case <-u.stop:
			return
		case <-t.C:
			u.ping()
		}
	}
}

// ping measures a round trip to the server, telling it what the uplink
// and OnPing measured before.
func (u *Uplink) ping() {
	if !u.pinging.CompareAndSwap(false, true) {
		return
	}
	defer u.pinging.Store(false)
	c, err := u.Client()
	if err != nil {
		return
	}
	req := proto.PingReq{RTT: u.rtt.Stats()}
	if u.OnPing != nil {
		u.OnPing(&req)
	}
	var rsp proto.PingRsp
	start := time.Now()
	err = c.Call(proto.MethodPing, &req, &rsp, PingTimeout)
	d := time.Since(start)
	if now, _ := u.Client(); now != c {
		// The connection the ping went over is gone, which says nothing of
		// the link.
		return
	}
	if err != nil {
		u.rtt.Lost()
		return
	}
	u.rtt.Add(d)
	if u.OnPong != nil {
		u.OnPong(u, &rsp)
	}
}

// RTT is the round trip to the server.
func (u *Uplink) RTT() latency.Stats { return u.rtt.Stats() }

// ErrNotRegistered is the error of a call on an uplink that is not
// registered.
var ErrNotRegistered = errors.New("fibhub: not registered with the server")

// linksLocked is the registered clients, by the places the server gave
// them. Callers hold mu.
func (u *Uplink) linksLocked() []*arpc.Client {
	links := make([]*arpc.Client, 0, len(u.conns))
	for c := range u.conns {
		links = append(links, c)
	}
	sort.Slice(links, func(i, j int) bool { return u.conns[links[i]].Index < u.conns[links[j]].Index })
	return links
}

// Client is the registered arpc client the server numbered lowest, which
// the uplink calls the server over.
func (u *Uplink) Client() (*arpc.Client, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	links := u.linksLocked()
	if u.stopped || len(links) == 0 {
		return nil, ErrNotRegistered
	}
	return links[0], nil
}

// NextTunnelID is the ID of a new tunnel, and the registered client of the
// pool that carries it: the one that carries the fewest tunnels, the
// clients taking turns among those that carry as few.
func (u *Uplink) NextTunnelID() (uint64, *arpc.Client, error) {
	load := u.Tunnels.Load()
	u.mu.Lock()
	defer u.mu.Unlock()
	links := u.linksLocked()
	if u.stopped || len(links) == 0 {
		return 0, nil, ErrNotRegistered
	}
	seq := u.seq.Add(1)
	link := links[int(seq%uint32(len(links)))]
	for _, c := range links {
		if load[c] < load[link] {
			link = c
		}
	}
	return proto.TunnelID(u.conns[link].Conn, seq), link, nil
}

// Call calls method on the server.
func (u *Uplink) Call(method string, req, rsp any) error {
	c, err := u.Client()
	if err != nil {
		return err
	}
	return c.Call(method, req, rsp, CallTimeout)
}

// Stop closes the connections and their tunnels, and dials no more.
func (u *Uplink) Stop() {
	u.mu.Lock()
	if u.stopped {
		u.mu.Unlock()
		return
	}
	u.stopped = true
	u.state = StateStopped
	close(u.stop)
	pool := u.pool
	u.mu.Unlock()
	u.Tunnels.CloseAll("stopped")
	for _, c := range pool {
		c.Stop()
	}
}

// Status is the uplink's state.
func (u *Uplink) Status() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := Status{Name: u.Config.Name, Addr: u.Config.Addr, State: u.state, Error: u.lastErr, Server: u.server, Since: u.since, Tunnels: u.Tunnels.Len(), RTT: u.rtt.Stats(), BodyEncrypted: u.Config.BodyPassword != "", PoolSize: u.Config.poolSize(), Places: []int{}}
	for i, c := range u.linksLocked() {
		rsp := u.conns[c]
		if i == 0 {
			st.PeerID = rsp.Peer
		}
		st.Places = append(st.Places, rsp.Index)
	}
	st.Pool = len(st.Places)
	return st
}
