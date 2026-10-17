//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Package server is fibhub's server mode. Pods and clients connect to its
// arpc address and register with the shared password, each with a pool of
// connections that join the one that registered; the server keeps track of
// them, routes each tunnel a client opens to the pod it names, and passes
// the tunnel's frames between the two, over one connection of each
// throughout, so they keep their order. A web page shows what is
// registered and lets an operator query and update pods, kick peers and
// close tunnels, and a WebSocket address carries that page, encrypted with
// a password, to whoever cannot reach it directly.
package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib/arpc"
	"github.com/lesismal/fibhub/internal/bodycrypt"
	"github.com/lesismal/fibhub/internal/latency"
	"github.com/lesismal/fibhub/internal/proto"
	"github.com/lesismal/fibhub/internal/wsarpc"
)

// Config configures a server.
type Config struct {
	// Mode is "server" in a config file.
	Mode string `json:"mode,omitempty"`
	// Name is the server's name, which peers are told.
	Name string `json:"name"`
	// Addr is the arpc address pods and clients connect to.
	Addr string `json:"addr"`
	// WebAddr is the address of the web page.
	WebAddr string `json:"webAddr"`
	// WebAuth, "user:password", protects the web page; empty leaves it open.
	WebAuth string `json:"webAuth"`
	// WSAddr is the address of the encrypted WebSocket that reaches the web
	// page; empty disables it.
	WSAddr string `json:"wsAddr"`
	// Password authenticates pods and clients.
	Password string `json:"password"`
	// WSPassword encrypts the WebSocket; it defaults to Password.
	WSPassword string `json:"wsPassword"`
	// ArpcWSAddr is an address pods and clients reach the arpc service on
	// over WebSocket, at ArpcWSPath, for networks that let only HTTP
	// through; empty disables it.
	ArpcWSAddr string `json:"arpcWsAddr"`
	// ArpcWSPath is the path of the arpc WebSocket; it defaults to
	// /fibhub.
	ArpcWSPath string `json:"arpcWsPath"`
	// BodyPassword, when set, has every arpc message's body encrypted with
	// AES under a key derived from it; pods and clients must be configured
	// with the same one.
	BodyPassword string `json:"bodyPassword"`
	// RegisterTimeout is how long a connection may take to register.
	RegisterTimeout time.Duration `json:"-"`
	// CallTimeout bounds the calls the server makes on pods.
	CallTimeout time.Duration `json:"-"`
}

func (cfg *Config) setDefaults() {
	if cfg.Name == "" {
		cfg.Name = "fibhub"
	}
	if cfg.Addr == "" {
		cfg.Addr = ":7000"
	}
	if cfg.WebAddr == "" {
		cfg.WebAddr = "127.0.0.1:7080"
	}
	if cfg.WSPassword == "" {
		cfg.WSPassword = cfg.Password
	}
	if cfg.RegisterTimeout <= 0 {
		cfg.RegisterTimeout = 10 * time.Second
	}
	if cfg.ArpcWSPath == "" {
		cfg.ArpcWSPath = "/fibhub"
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = 15 * time.Second
	}
}

// peerKey is where a connection's Peer is kept on its arpc client.
type peerKey struct{}

// Peer is a registered pod or client.
type Peer struct {
	ID uint32
	// Client is the connection the peer registered on, which the server
	// calls the peer on.
	Client *arpc.Client
	Addr   string
	Since  time.Time

	// session is what the peer's other connections join it with, and links
	// the connections that carry its tunnels, Client first; links change
	// under the server's lock.
	session string
	links   []*arpc.Client

	mu   sync.Mutex
	info proto.Info
	// rtt and nodeRTT are what the peer's last ping told.
	rtt     latency.Stats
	nodeRTT map[string]latency.Stats
}

// Info is what the peer told about itself.
func (p *Peer) Info() proto.Info {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.info
}

func (p *Peer) setInfo(info proto.Info) {
	p.mu.Lock()
	p.info = info
	p.mu.Unlock()
}

// Latency is the round trip between the server and the peer and, for a
// pod, how long its connects to its nodes took, as the peer measured them.
func (p *Peer) Latency() proto.PodLatency {
	p.mu.Lock()
	defer p.mu.Unlock()
	return proto.PodLatency{RTT: p.rtt, NodeRTT: p.nodeRTT}
}

func (p *Peer) setLatency(req *proto.PingReq) {
	p.mu.Lock()
	p.rtt, p.nodeRTT = req.RTT, req.NodeRTT
	p.mu.Unlock()
}

func (p *Peer) key() string {
	info := p.Info()
	return info.Type + "/" + info.Name
}

// Route is a tunnel the server passes frames along.
type Route struct {
	ID     uint64
	Client *Peer
	Pod    *Peer
	// ClientLink and PodLink are the connections of Client and Pod that
	// carry the tunnel.
	ClientLink, PodLink *arpc.Client
	Node                string
	// Addr is the node's address, as the pod reports it once the tunnel is
	// open; it changes under the server's lock.
	Addr     string
	Created  time.Time
	up, down atomic.Int64
	open     atomic.Bool
}

// Server is a running server.
type Server struct {
	cfg      Config
	out      io.Writer
	verifier *proto.Verifier
	arpc     *arpc.Server
	engine   *fib.Engine
	webAddr  string
	// webListen is the address the web page listens on, as bound.
	webListen string
	// wsIndex and arpcWSIndex are the bindings of the WebSockets, or -1.
	wsIndex, arpcWSIndex int

	mu      sync.Mutex
	nextID  uint32
	peers   map[*arpc.Client]*Peer
	byName  map[string]*Peer
	byID    map[uint32]*Peer
	routes  map[uint64]*Route
	started time.Time
}

// Run serves cfg until ctx ends. ready, if not nil, is told the server once
// it is listening.
func Run(ctx context.Context, cfg Config, out io.Writer, ready func(*Server)) error {
	cfg.setDefaults()
	if cfg.Password == "" {
		return errors.New("server: a password is required")
	}
	s := &Server{
		cfg:      cfg,
		out:      out,
		verifier: &proto.Verifier{Password: cfg.Password},
		peers:    map[*arpc.Client]*Peer{},
		byName:   map[string]*Peer{},
		byID:     map[uint32]*Peer{},
		routes:   map[uint64]*Route{},
		started:  time.Now(),
	}
	s.arpc = arpc.NewServer()
	s.setupArpc(s.arpc.Handler)
	if cfg.BodyPassword != "" {
		coder, err := bodycrypt.New(cfg.BodyPassword)
		if err != nil {
			return err
		}
		s.arpc.Handler.UseCoder(coder)
	}

	bindings := []fib.Binding{
		{Addr: cfg.Addr, Handler: s.arpc},
		{Addr: cfg.WebAddr, Handler: s.webHandler()},
	}
	engine, err := fib.New(fib.DefaultConfig(), bindings...)
	if err != nil {
		return err
	}
	s.engine = engine
	s.webAddr = dialable(engine, 1)
	if addrs, err := engine.BindingAddrs(1); err == nil && len(addrs) > 0 {
		s.webListen = addrs[0].String()
	}
	s.wsIndex, s.arpcWSIndex = -1, -1
	if cfg.WSAddr != "" {
		if s.wsIndex, err = engine.AddBinding(fib.Binding{Addr: cfg.WSAddr, Handler: s.wsHandler()}); err != nil {
			_ = engine.Close()
			return err
		}
	}
	if cfg.ArpcWSAddr != "" {
		if s.arpcWSIndex, err = engine.AddBinding(fib.Binding{Addr: cfg.ArpcWSAddr, Handler: wsarpc.NewServer(s.arpc, cfg.ArpcWSPath)}); err != nil {
			_ = engine.Close()
			return err
		}
	}
	running := make(chan error, 1)
	go func() { running <- engine.Run() }()

	fmt.Fprintf(out, "fibhub server %q: arpc on %s, web on http://%s", cfg.Name, s.Addr(), s.WebAddr())
	if cfg.ArpcWSAddr != "" {
		fmt.Fprintf(out, ", arpc over websocket on %s", s.ArpcWSURL())
	}
	if cfg.WSAddr != "" {
		fmt.Fprintf(out, ", web page over websocket on ws://%s", s.WSAddr())
	}
	if cfg.BodyPassword != "" {
		fmt.Fprint(out, ", arpc bodies encrypted")
	}
	fmt.Fprintln(out)
	if ready != nil {
		ready(s)
	}

	select {
	case <-ctx.Done():
	case err = <-running:
		_ = engine.Close()
		return err
	}
	engine.Stop()
	err = <-running
	_ = engine.Close()
	return err
}

// dialable is the address the i-th binding of engine can be dialed on: its
// address, with an unspecified host replaced by the loopback.
func dialable(engine *fib.Engine, i int) string {
	addrs, err := engine.BindingAddrs(i)
	if err != nil || len(addrs) == 0 {
		return ""
	}
	tcp, ok := addrs[0].(*net.TCPAddr)
	if !ok {
		return addrs[0].String()
	}
	ip := tcp.IP
	if ip == nil || ip.IsUnspecified() {
		// A listener on every address of both families takes IPv4 too.
		ip = net.IPv4(127, 0, 0, 1)
	}
	return net.JoinHostPort(ip.String(), fmt.Sprint(tcp.Port))
}

// Addr is the arpc address.
func (s *Server) Addr() string { return dialable(s.engine, 0) }

// WebAddr is the web page's address.
func (s *Server) WebAddr() string { return s.webAddr }

// WSAddr is the WebSocket's address, or "" without one.
func (s *Server) WSAddr() string {
	if s.wsIndex < 0 {
		return ""
	}
	return dialable(s.engine, s.wsIndex)
}

// ArpcWSURL is the URL of the arpc WebSocket, or "" without one.
func (s *Server) ArpcWSURL() string {
	if s.arpcWSIndex < 0 {
		return ""
	}
	return "ws://" + dialable(s.engine, s.arpcWSIndex) + s.cfg.ArpcWSPath
}

func peerOf(c *arpc.Client) *Peer {
	if v, ok := c.Get(peerKey{}); ok {
		return v.(*Peer)
	}
	return nil
}

var errNotRegistered = errors.New("fibhub: register first")

// setupArpc registers the methods pods and clients call.
func (s *Server) setupArpc(h *arpc.Handler) {
	// Only registration is served before a connection has registered.
	h.Use(func(ctx *arpc.Context) {
		if m := ctx.Message.Method(); m != proto.MethodRegister && m != proto.MethodJoin && peerOf(ctx.Client) == nil {
			_ = ctx.Error(errNotRegistered)
			ctx.Abort()
		}
	})
	h.HandleConnected(func(c *arpc.Client) {
		if conn := c.Conn(); conn != nil {
			// The tunnels' windows bound what is queued; a connection that
			// stopped reading for its own backlog could wait on its peer
			// doing the same.
			conn.SetWriteBackpressure(false)
		}
		time.AfterFunc(s.cfg.RegisterTimeout, func() {
			if peerOf(c) == nil {
				if conn := c.Conn(); conn != nil {
					_ = conn.Close()
				}
			}
		})
	})
	h.HandleDisconnected(s.disconnected)

	h.Handle(proto.MethodRegister, s.register)
	h.Handle(proto.MethodJoin, s.join)
	h.Handle(proto.MethodUpdate, s.update)
	h.Handle(proto.MethodPing, s.ping, false)
	h.Handle(proto.MethodPods, func(ctx *arpc.Context) { _ = ctx.Write(s.podList()) })
	h.Handle(proto.MethodSetPodNodes, func(ctx *arpc.Context) {
		var req proto.SetNodesReq
		if err := ctx.Bind(&req); err != nil {
			_ = ctx.Error(err)
			return
		}
		pod := s.peerByName(proto.TypePod, req.Pod)
		if pod == nil {
			_ = ctx.Error(fmt.Errorf("fibhub: no pod %q", req.Pod))
			return
		}
		info, err := s.setPodNodes(pod, req.Nodes)
		if err != nil {
			_ = ctx.Error(err)
			return
		}
		_ = ctx.Write(&info)
	})
	h.Handle(proto.MethodTunnelOpen, s.openTunnel)
	h.Handle(proto.MethodTunnelData, func(ctx *arpc.Context) { s.forward(ctx, proto.MethodTunnelData) }, false)
	h.Handle(proto.MethodTunnelAck, func(ctx *arpc.Context) { s.forward(ctx, proto.MethodTunnelAck) }, false)
	h.Handle(proto.MethodTunnelClose, func(ctx *arpc.Context) { s.forward(ctx, proto.MethodTunnelClose) }, false)
}

func (s *Server) register(ctx *arpc.Context) {
	var req proto.RegisterReq
	if err := ctx.Bind(&req); err != nil {
		_ = ctx.Error(err)
		return
	}
	if peerOf(ctx.Client) != nil {
		_ = ctx.Error(errors.New("fibhub: already registered"))
		return
	}
	if err := s.verifier.Verify(&req); err != nil {
		slog.Warn("fibhub: registration refused", "remote", remote(ctx.Client), "name", req.Info.Name, "err", err)
		_ = ctx.Error(err)
		return
	}
	peer := &Peer{Client: ctx.Client, Addr: remote(ctx.Client), Since: time.Now(), session: proto.NewSession(), links: []*arpc.Client{ctx.Client}, info: req.Info}
	s.mu.Lock()
	s.nextID++
	if s.nextID == 0 {
		s.nextID++
	}
	peer.ID = s.nextID
	old := s.byName[peer.key()]
	s.byName[peer.key()] = peer
	s.byID[peer.ID] = peer
	s.peers[ctx.Client] = peer
	s.mu.Unlock()
	ctx.Client.Set(peerKey{}, peer)
	if old != nil {
		// The newer connection wins: the old one is likely a connection the
		// peer lost without the server hearing of it yet.
		s.kick(old, "replaced by a new connection from "+peer.Addr)
	}
	slog.Info("fibhub: registered", "type", req.Info.Type, "name", req.Info.Name, "id", peer.ID, "remote", peer.Addr)
	_ = ctx.Write(&proto.RegisterRsp{ID: peer.ID, Server: s.cfg.Name, Session: peer.session})
	if req.Info.Type == proto.TypePod {
		s.broadcastPods()
	}
}

var errNoJoin = errors.New("fibhub: no such registration to join")

// join adds the connection to the registered peer it names, to carry some
// of its tunnels.
func (s *Server) join(ctx *arpc.Context) {
	var req proto.JoinReq
	if err := ctx.Bind(&req); err != nil {
		_ = ctx.Error(err)
		return
	}
	if peerOf(ctx.Client) != nil {
		_ = ctx.Error(errors.New("fibhub: already registered"))
		return
	}
	if err := s.verifier.Verify(&req.RegisterReq); err != nil {
		slog.Warn("fibhub: join refused", "remote", remote(ctx.Client), "name", req.Info.Name, "err", err)
		_ = ctx.Error(err)
		return
	}
	s.mu.Lock()
	peer := s.byID[req.ID]
	if peer == nil || peer.key() != req.Info.Type+"/"+req.Info.Name || subtle.ConstantTimeCompare([]byte(peer.session), []byte(req.Session)) != 1 {
		s.mu.Unlock()
		_ = ctx.Error(errNoJoin)
		return
	}
	peer.links = append(peer.links, ctx.Client)
	s.peers[ctx.Client] = peer
	s.mu.Unlock()
	ctx.Client.Set(peerKey{}, peer)
	_ = ctx.Write("ok")
}

func (s *Server) update(ctx *arpc.Context) {
	peer := peerOf(ctx.Client)
	var info proto.Info
	if err := ctx.Bind(&info); err != nil {
		_ = ctx.Error(err)
		return
	}
	old := peer.Info()
	if info.Name != old.Name || info.Type != old.Type {
		_ = ctx.Error(errors.New("fibhub: a peer cannot change its name or type"))
		return
	}
	peer.setInfo(info)
	_ = ctx.Write("ok")
	if info.Type == proto.TypePod {
		s.broadcastPods()
	}
}

// ping records what a peer measured, and tells a client how fast the pods'
// links are. It is handled where the connection is read, so its round trip
// includes the wait behind the tunnel frames read before it.
func (s *Server) ping(ctx *arpc.Context) {
	peer := peerOf(ctx.Client)
	var req proto.PingReq
	if err := ctx.Bind(&req); err != nil {
		_ = ctx.Error(err)
		return
	}
	peer.setLatency(&req)
	var rsp proto.PingRsp
	if peer.Info().Type == proto.TypeClient {
		rsp.Pods = map[string]proto.PodLatency{}
		for _, p := range s.Peers() {
			if info := p.Info(); info.Type == proto.TypePod {
				rsp.Pods[info.Name] = p.Latency()
			}
		}
	}
	_ = ctx.Write(&rsp)
}

// disconnected closes the tunnels a connection that closed carried. A
// peer whose registering connection closed is forgotten, and its other
// connections closed.
func (s *Server) disconnected(c *arpc.Client) {
	s.mu.Lock()
	peer := s.peers[c]
	if peer == nil {
		s.mu.Unlock()
		return
	}
	delete(s.peers, c)
	if c != peer.Client {
		peer.links = slices.DeleteFunc(peer.links, func(l *arpc.Client) bool { return l == c })
		routes := s.takeRoutesLocked(func(r *Route) bool { return r.ClientLink == c || r.PodLink == c })
		s.mu.Unlock()
		info := peer.Info()
		closeRoutes(routes, peer, "a connection of "+info.Type+" "+info.Name+" closed")
		return
	}
	others := slices.DeleteFunc(peer.links, func(l *arpc.Client) bool { return l == c })
	peer.links = nil
	for _, l := range others {
		delete(s.peers, l)
	}
	delete(s.byID, peer.ID)
	if s.byName[peer.key()] == peer {
		delete(s.byName, peer.key())
	}
	routes := s.takeRoutesLocked(func(r *Route) bool { return r.Client == peer || r.Pod == peer })
	s.mu.Unlock()
	for _, l := range others {
		if conn := l.Conn(); conn != nil {
			_ = conn.Close()
		}
	}
	info := peer.Info()
	slog.Info("fibhub: disconnected", "type", info.Type, "name", info.Name, "id", peer.ID, "tunnels", len(routes))
	closeRoutes(routes, peer, info.Type+" "+info.Name+" disconnected")
	if info.Type == proto.TypePod {
		s.broadcastPods()
	}
}

// takeRoutesLocked removes the routes match picks, and returns them.
// Callers hold mu.
func (s *Server) takeRoutesLocked(match func(*Route) bool) []*Route {
	var routes []*Route
	for id, r := range s.routes {
		if match(r) {
			r.open.Store(false)
			routes = append(routes, r)
			delete(s.routes, id)
		}
	}
	return routes
}

// closeRoutes tells the other ends of peer's routes, which the server has
// removed, that they are closed.
func closeRoutes(routes []*Route, peer *Peer, reason string) {
	for _, r := range routes {
		other := r.PodLink
		if r.Pod == peer {
			other = r.ClientLink
		}
		notifyClose(other, r.ID, reason)
	}
}

// kick tells peer why and closes its connection.
func (s *Server) kick(peer *Peer, reason string) {
	_ = peer.Client.Notify(proto.MethodKick, reason, time.Second)
	if conn := peer.Client.Conn(); conn != nil {
		conn.CloseAfterSend()
	}
}

// notifyClose closes tunnel id at the end link carries.
func notifyClose(link *arpc.Client, id uint64, reason string) {
	_ = link.Notify(proto.MethodTunnelClose, proto.Frame(id, []byte(reason)), time.Second)
}

func remote(c *arpc.Client) string {
	if conn := c.Conn(); conn != nil {
		if a := conn.RemoteAddr(); a != nil {
			return a.String()
		}
	}
	return ""
}

func (s *Server) peerByName(typ, name string) *Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byName[typ+"/"+name]
}

func (s *Server) peerByID(id uint32) *Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byID[id]
}

// Peers is every registered peer, by ID.
func (s *Server) Peers() []*Peer {
	s.mu.Lock()
	peers := make([]*Peer, 0, len(s.byID))
	for _, p := range s.byID {
		peers = append(peers, p)
	}
	s.mu.Unlock()
	sort.Slice(peers, func(i, j int) bool { return peers[i].ID < peers[j].ID })
	return peers
}

// Routes is every open tunnel, by ID.
func (s *Server) Routes() []*Route {
	s.mu.Lock()
	routes := make([]*Route, 0, len(s.routes))
	for _, r := range s.routes {
		routes = append(routes, r)
	}
	s.mu.Unlock()
	sort.Slice(routes, func(i, j int) bool { return routes[i].ID < routes[j].ID })
	return routes
}

func (s *Server) podList() *proto.PodList {
	list := &proto.PodList{Server: s.cfg.Name, Pods: []proto.PodView{}}
	for _, p := range s.Peers() {
		if info := p.Info(); info.Type == proto.TypePod {
			list.Pods = append(list.Pods, proto.PodView{Info: info, Addr: p.Addr, Since: p.Since, PodLatency: p.Latency()})
		}
	}
	sort.Slice(list.Pods, func(i, j int) bool { return list.Pods[i].Info.Name < list.Pods[j].Info.Name })
	return list
}

// broadcastPods sends every client the pods.
func (s *Server) broadcastPods() {
	list := s.podList()
	for _, p := range s.Peers() {
		if p.Info().Type == proto.TypeClient {
			_ = p.Client.Notify(proto.MethodPods, list, time.Second)
		}
	}
}

// refreshPod asks pod for its info.
func (s *Server) refreshPod(pod *Peer) (proto.Info, error) {
	var info proto.Info
	if err := pod.Client.Call(proto.MethodPodInfo, nil, &info, s.cfg.CallTimeout); err != nil {
		return info, err
	}
	if err := s.acceptPodInfo(pod, info); err != nil {
		return info, err
	}
	return info, nil
}

// setPodNodes has pod reach nodes.
func (s *Server) setPodNodes(pod *Peer, nodes []proto.Node) (proto.Info, error) {
	if nodes == nil {
		nodes = []proto.Node{}
	}
	var info proto.Info
	if err := pod.Client.Call(proto.MethodPodSetNodes, nodes, &info, s.cfg.CallTimeout); err != nil {
		return info, err
	}
	if err := s.acceptPodInfo(pod, info); err != nil {
		return info, err
	}
	return info, nil
}

func (s *Server) acceptPodInfo(pod *Peer, info proto.Info) error {
	old := pod.Info()
	if info.Name != old.Name || info.Type != old.Type {
		return errors.New("fibhub: the pod answered with another name or type")
	}
	pod.setInfo(info)
	s.broadcastPods()
	return nil
}

// openTunnel opens the tunnel a client asks for, on the pod it names.
func (s *Server) openTunnel(ctx *arpc.Context) {
	client := peerOf(ctx.Client)
	var req proto.TunnelOpenReq
	if err := ctx.Bind(&req); err != nil {
		_ = ctx.Error(err)
		return
	}
	if client.Info().Type != proto.TypeClient {
		_ = ctx.Error(errors.New("fibhub: only a client opens tunnels"))
		return
	}
	if proto.TunnelPeer(req.ID) != client.ID {
		_ = ctx.Error(errors.New("fibhub: a tunnel ID must carry the client's ID"))
		return
	}
	pod := s.peerByName(proto.TypePod, req.Pod)
	if pod == nil {
		_ = ctx.Error(fmt.Errorf("fibhub: no pod %q", req.Pod))
		return
	}
	info := pod.Info()
	node, ok := info.FindNode(req.Node)
	if !ok {
		_ = ctx.Error(fmt.Errorf("fibhub: pod %q has no node %q", req.Pod, req.Node))
		return
	}
	r := &Route{ID: req.ID, Client: client, Pod: pod, ClientLink: ctx.Client, Node: req.Node, Addr: node.Addr, Created: time.Now()}
	r.open.Store(true)
	s.mu.Lock()
	if _, taken := s.routes[r.ID]; taken {
		s.mu.Unlock()
		_ = ctx.Error(errors.New("fibhub: tunnel ID in use"))
		return
	}
	if r.PodLink = s.pickLinkLocked(pod); r.PodLink == nil {
		s.mu.Unlock()
		_ = ctx.Error(fmt.Errorf("fibhub: pod %q disconnected", req.Pod))
		return
	}
	s.routes[r.ID] = r
	s.mu.Unlock()

	req.Client = client.Info().Name
	var rsp proto.TunnelOpenRsp
	// The pod's end of the tunnel goes on the connection the pod is asked
	// on.
	if err := r.PodLink.Call(proto.MethodTunnelOpen, &req, &rsp, s.cfg.CallTimeout); err != nil {
		s.removeRoute(r)
		_ = ctx.Error(err)
		return
	}
	if rsp.Addr != "" {
		// The route is listed already, so the web page may be reading it.
		s.mu.Lock()
		r.Addr = rsp.Addr
		s.mu.Unlock()
	}
	if !r.open.Load() {
		// The client gave up while the pod connected.
		notifyClose(r.PodLink, r.ID, "closed while opening")
		_ = ctx.Error(errors.New("fibhub: tunnel closed while opening"))
		return
	}
	slog.Info("fibhub: tunnel open", "id", r.ID, "client", req.Client, "pod", req.Pod, "node", req.Node, "addr", r.Addr)
	_ = ctx.Write(&rsp)
}

// pickLinkLocked is the connection of peer a new tunnel goes on: the one
// that carries the fewest tunnels, the first of those that carry as few.
// Callers hold mu.
func (s *Server) pickLinkLocked(peer *Peer) *arpc.Client {
	if len(peer.links) == 0 {
		return nil
	}
	load := map[*arpc.Client]int{}
	for _, r := range s.routes {
		if r.Pod == peer {
			load[r.PodLink]++
		}
	}
	link := peer.links[0]
	for _, l := range peer.links[1:] {
		if load[l] < load[link] {
			link = l
		}
	}
	return link
}

func (s *Server) route(id uint64) *Route {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.routes[id]
}

func (s *Server) removeRoute(r *Route) bool {
	r.open.Store(false)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.routes[r.ID] == r {
		delete(s.routes, r.ID)
		return true
	}
	return false
}

// forward passes a tunnel frame to the other end of its tunnel.
func (s *Server) forward(ctx *arpc.Context, method string) {
	body := ctx.Body()
	id, payload, err := proto.ParseFrame(body)
	if err != nil {
		return
	}
	r := s.route(id)
	if r == nil {
		return
	}
	// Each end sends on the connection that carries the tunnel, and the
	// frames go on along the other end's, in the order they came.
	var to *arpc.Client
	switch ctx.Client {
	case r.ClientLink:
		to = r.PodLink
		if method == proto.MethodTunnelData {
			r.up.Add(int64(len(payload)))
		}
	case r.PodLink:
		to = r.ClientLink
		if method == proto.MethodTunnelData {
			r.down.Add(int64(len(payload)))
		}
	default:
		return
	}
	_ = to.Notify(method, body, time.Second)
	if method == proto.MethodTunnelClose {
		s.removeRoute(r)
	}
}

// CloseRoute closes tunnel id at both ends.
func (s *Server) CloseRoute(id uint64, reason string) bool {
	r := s.route(id)
	if r == nil || !s.removeRoute(r) {
		return false
	}
	notifyClose(r.ClientLink, id, reason)
	notifyClose(r.PodLink, id, reason)
	return true
}
