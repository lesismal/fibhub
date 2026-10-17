// Package uplink is a pod's or client's connection to a fibhub server: a
// pool of arpc clients that keep dialing until they connect. The first
// registers itself each time it connects, pings the server to measure the
// link, and closes the tunnels the pool carried when it loses the server;
// the others join its registration each time they connect. Each tunnel goes
// over one client of the pool throughout, so its frames keep their order,
// and closes when that client loses the server.
package uplink

import (
	"errors"
	"log/slog"
	"slices"
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
	// PoolSize is how many connections the uplink makes, and Pool how many
	// of them carry tunnels now.
	PoolSize int `json:"poolSize"`
	Pool     int `json:"pool"`
}

// Uplink is the connection to one server.
type Uplink struct {
	Config  Config
	Tunnels *tunnel.Table
	// OnRegistered, if set, runs each time the uplink registers.
	OnRegistered func(*Uplink)
	// OnPing, if set, adds to each ping what else the peer measured.
	OnPing func(*proto.PingReq)
	// OnPong, if set, gets the server's answer to each ping.
	OnPong func(*Uplink, *proto.PingRsp)

	engine *fib.Engine
	info   func() proto.Info
	// handler is the registering client's, pooled the other clients'.
	handler, pooled *arpc.Handler
	seq             atomic.Uint32
	rtt             latency.Meter
	pinging         atomic.Bool

	mu sync.Mutex
	// client is the client that registers, pool every client dialed, and
	// joined the clients other than client that joined its registration.
	client  *arpc.Client
	pool    []*arpc.Client
	joined  []*arpc.Client
	state   string
	lastErr string
	peerID  uint32
	session string
	server  string
	since   time.Time
	// registered is closed, and replaced, each time the uplink registers.
	registered chan struct{}
	stopped    bool
	stop       chan struct{}
}

// New is an uplink to the server cfg names, which registers as what info
// returns. setup registers the methods the server may call on it.
func New(engine *fib.Engine, cfg Config, info func() proto.Info, setup func(u *Uplink, h *arpc.Handler)) *Uplink {
	if cfg.Name == "" {
		cfg.Name = cfg.Addr
	}
	u := &Uplink{Config: cfg, Tunnels: tunnel.NewTable(), engine: engine, info: info, state: StateConnecting, registered: make(chan struct{}), stop: make(chan struct{})}
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
		u.fail("kicked by the server: " + string(ctx.Body()))
	}, false)
	if setup != nil {
		setup(u, h)
	}
	// The pool's other clients serve what the registering one does, but
	// join its registration rather than register.
	p := h.Clone()
	p.HandleConnected(func(c *arpc.Client) { go u.join(c) })
	p.HandleReconnect(func(c *arpc.Client, info *arpc.ReconnectInfo) {
		if info.Times == 1 {
			u.leave(c, "server connection lost")
		}
	})
	u.pooled = p
	h.HandleConnected(func(c *arpc.Client) { go u.register(c) })
	h.HandleReconnect(func(_ *arpc.Client, info *arpc.ReconnectInfo) {
		if info.Times == 1 {
			u.lost()
		}
		if !info.Success && info.Err != nil {
			u.fail(info.Err.Error())
		}
	})
	u.handler = h
	return u
}

// Start dials the server, and goes on dialing until it connects or the
// uplink stops, and then the rest of the pool likewise; the arpc clients
// reconnect on their own from then on.
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
			u.fail(err.Error())
			return
		}
		dialer = d
	}
	go u.pingLoop()
	go func() {
		if !u.dial(dialer, u.handler) {
			return
		}
		for range u.Config.poolSize() - 1 {
			go u.dial(dialer, u.pooled)
		}
	}()
}

// dial dials a client of the pool with h, and goes on dialing until it
// connects or the uplink stops, reporting whether it connected.
func (u *Uplink) dial(dialer arpc.DialerFunc, h *arpc.Handler) bool {
	for {
		c, err := arpc.NewClient(dialer, h)
		u.mu.Lock()
		if u.stopped {
			u.mu.Unlock()
			if c != nil {
				c.Stop()
			}
			return false
		}
		if err == nil {
			u.pool = append(u.pool, c)
			if h == u.handler {
				u.client = c
			}
			u.mu.Unlock()
			return true
		}
		if h == u.handler {
			u.state, u.lastErr = StateFailed, err.Error()
		}
		u.mu.Unlock()
		select {
		case <-u.stop:
			return false
		case <-time.After(RetryInterval):
		}
	}
}

// register authenticates and registers on c, which has just connected.
func (u *Uplink) register(c *arpc.Client) {
	// Nothing of a connection before this one goes on.
	u.Tunnels.CloseAll("reconnected")
	u.rtt.Reset()
	req := proto.RegisterReq{Info: u.info()}
	proto.Sign(&req, u.Config.Password)
	var rsp proto.RegisterRsp
	if err := c.Call(proto.MethodRegister, &req, &rsp, CallTimeout); err != nil {
		u.fail("register: " + err.Error())
		slog.Warn("fibhub: register failed", "server", u.Config.Addr, "err", err)
		// The arpc client dials again after its interval.
		closeConn(c)
		return
	}
	u.mu.Lock()
	if u.stopped {
		u.mu.Unlock()
		return
	}
	u.client = c
	u.state, u.lastErr = StateRegistered, ""
	u.peerID, u.session, u.server, u.since = rsp.ID, rsp.Session, rsp.Server, time.Now()
	close(u.registered)
	u.registered = make(chan struct{})
	f := u.OnRegistered
	u.mu.Unlock()
	slog.Info("fibhub: registered", "server", u.Config.Addr, "name", rsp.Server, "id", rsp.ID)
	if f != nil {
		f(u)
	}
	// Measure the link at once, rather than an interval from now.
	go u.ping()
}

// join has c, a client of the pool other than the one that registers, which
// has just connected, join the uplink's registration once there is one.
func (u *Uplink) join(c *arpc.Client) {
	conn := c.Conn()
	// Nothing of a connection before this one goes on.
	u.leave(c, "reconnected")
	for {
		u.mu.Lock()
		stopped, id, session, wait := u.stopped, u.peerID, u.session, u.registered
		u.mu.Unlock()
		if stopped || conn == nil || c.Conn() != conn {
			return
		}
		if id != 0 {
			err := u.joinAs(c, conn, id, session)
			if err == nil {
				return
			}
			slog.Warn("fibhub: join failed", "server", u.Config.Addr, "err", err)
			wait = nil
		}
		select {
		case <-u.stop:
			return
		case <-wait:
		case <-time.After(RetryInterval):
		}
	}
}

// joinAs has c, over conn, join registration id with session.
func (u *Uplink) joinAs(c *arpc.Client, conn *fib.Connection, id uint32, session string) error {
	info := u.info()
	req := proto.JoinReq{RegisterReq: proto.RegisterReq{Info: proto.Info{Name: info.Name, Type: info.Type}}, ID: id, Session: session}
	proto.Sign(&req.RegisterReq, u.Config.Password)
	if err := c.Call(proto.MethodJoin, &req, nil, CallTimeout); err != nil {
		return err
	}
	u.mu.Lock()
	if u.stopped || u.peerID != id || u.session != session || c.Conn() != conn {
		u.mu.Unlock()
		// The registration it joined is gone already; it reconnects and
		// joins the next one.
		_ = conn.Close()
		return nil
	}
	u.joined = append(u.joined, c)
	u.mu.Unlock()
	return nil
}

// leave takes c, a client of the pool other than the one that registers,
// out of the registration, and closes the tunnels it carried.
func (u *Uplink) leave(c *arpc.Client, reason string) {
	u.mu.Lock()
	u.joined = slices.DeleteFunc(u.joined, func(j *arpc.Client) bool { return j == c })
	u.mu.Unlock()
	u.Tunnels.CloseLink(c, reason)
}

func closeConn(c *arpc.Client) {
	if conn := c.Conn(); conn != nil {
		_ = conn.Close()
	}
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

// lost records the loss of the registering client's connection, and with
// it the registration.
func (u *Uplink) lost() {
	u.mu.Lock()
	if u.state == StateRegistered {
		u.state = StateConnecting
	}
	u.peerID, u.session = 0, ""
	joined := u.joined
	u.joined = nil
	u.mu.Unlock()
	u.rtt.Reset()
	u.Tunnels.CloseAll("server connection lost")
	// The clients that joined the registration reconnect, and join the
	// next one.
	for _, c := range joined {
		closeConn(c)
	}
}

func (u *Uplink) fail(msg string) {
	u.mu.Lock()
	if !u.stopped {
		u.state, u.lastErr = StateFailed, msg
	}
	u.mu.Unlock()
}

// ErrNotRegistered is the error of a call on an uplink that is not
// registered.
var ErrNotRegistered = errors.New("fibhub: not registered with the server")

// Client is the arpc client of a registered uplink.
func (u *Uplink) Client() (*arpc.Client, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.state != StateRegistered || u.client == nil {
		return nil, ErrNotRegistered
	}
	return u.client, nil
}

// NextTunnelID is the ID of a new tunnel, and the client of the pool that
// carries it: the one that carries the fewest tunnels, the clients taking
// turns among those that carry as few.
func (u *Uplink) NextTunnelID() (uint64, *arpc.Client, error) {
	load := u.Tunnels.Load()
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.state != StateRegistered || u.client == nil {
		return 0, nil, ErrNotRegistered
	}
	seq := u.seq.Add(1)
	links := append([]*arpc.Client{u.client}, u.joined...)
	link := links[int(seq%uint32(len(links)))]
	for _, c := range links {
		if load[c] < load[link] {
			link = c
		}
	}
	return proto.TunnelID(u.peerID, seq), link, nil
}

// Call calls method on the server.
func (u *Uplink) Call(method string, req, rsp any) error {
	c, err := u.Client()
	if err != nil {
		return err
	}
	return c.Call(method, req, rsp, CallTimeout)
}

// Stop closes the connection and its tunnels, and dials no more.
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
	pool := 0
	if u.state == StateRegistered {
		pool = 1 + len(u.joined)
	}
	return Status{Name: u.Config.Name, Addr: u.Config.Addr, State: u.state, Error: u.lastErr, Server: u.server, PeerID: u.peerID, Since: u.since, Tunnels: u.Tunnels.Len(), RTT: u.rtt.Stats(), BodyEncrypted: u.Config.BodyPassword != "", PoolSize: u.Config.poolSize(), Pool: pool}
}
