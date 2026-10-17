// Package uplink is a pod's or client's connection to a fibhub server: an
// arpc client that keeps dialing until it connects, registers itself each
// time it connects, pings the server to measure the link, and closes the
// tunnels it carried when it loses the server.
package uplink

import (
	"errors"
	"log/slog"
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

	engine  *fib.Engine
	info    func() proto.Info
	handler *arpc.Handler
	seq     atomic.Uint32
	rtt     latency.Meter
	pinging atomic.Bool

	mu      sync.Mutex
	client  *arpc.Client
	state   string
	lastErr string
	peerID  uint32
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
	u := &Uplink{Config: cfg, Tunnels: tunnel.NewTable(), engine: engine, info: info, state: StateConnecting, stop: make(chan struct{})}
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
// uplink stops; the arpc client reconnects on its own from then on.
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
				u.client = c
				u.mu.Unlock()
				return
			}
			u.state, u.lastErr = StateFailed, err.Error()
			u.mu.Unlock()
			select {
			case <-u.stop:
				return
			case <-time.After(RetryInterval):
			}
		}
	}()
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
		if conn := c.Conn(); conn != nil {
			_ = conn.Close()
		}
		return
	}
	u.mu.Lock()
	if u.stopped {
		u.mu.Unlock()
		return
	}
	u.client = c
	u.state, u.lastErr = StateRegistered, ""
	u.peerID, u.server, u.since = rsp.ID, rsp.Server, time.Now()
	f := u.OnRegistered
	u.mu.Unlock()
	slog.Info("fibhub: registered", "server", u.Config.Addr, "name", rsp.Server, "id", rsp.ID)
	if f != nil {
		f(u)
	}
	// Measure the link at once, rather than an interval from now.
	go u.ping()
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

// lost records the loss of the server's connection.
func (u *Uplink) lost() {
	u.mu.Lock()
	if u.state == StateRegistered {
		u.state = StateConnecting
	}
	u.peerID = 0
	u.mu.Unlock()
	u.rtt.Reset()
	u.Tunnels.CloseAll("server connection lost")
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

// NextTunnelID is the ID of a new tunnel, and the client that carries it.
func (u *Uplink) NextTunnelID() (uint64, *arpc.Client, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.state != StateRegistered || u.client == nil {
		return 0, nil, ErrNotRegistered
	}
	return proto.TunnelID(u.peerID, u.seq.Add(1)), u.client, nil
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
	c := u.client
	u.mu.Unlock()
	u.Tunnels.CloseAll("stopped")
	if c != nil {
		c.Stop()
	}
}

// Status is the uplink's state.
func (u *Uplink) Status() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	return Status{Name: u.Config.Name, Addr: u.Config.Addr, State: u.state, Error: u.lastErr, Server: u.server, PeerID: u.peerID, Since: u.since, Tunnels: u.Tunnels.Len(), RTT: u.rtt.Stats(), BodyEncrypted: u.Config.BodyPassword != ""}
}
