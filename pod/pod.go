//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Package pod is fibhub's pod mode. A pod connects to one or more fibhub
// servers, registers with the nodes it can reach, and opens the TCP
// connections to those nodes that the tunnels the servers route to it ask
// for. A web page shows its servers and tunnels and configures which servers
// it connects to and which nodes it can reach; a pod given a config file
// saves those settings there.
package pod

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib/arpc"
	"github.com/lesismal/fibhub/internal/common"
	"github.com/lesismal/fibhub/internal/latency"
	"github.com/lesismal/fibhub/internal/proto"
	"github.com/lesismal/fibhub/internal/tunnel"
	"github.com/lesismal/fibhub/internal/uplink"
)

// Config is a pod's settings, as its config file holds them.
type Config struct {
	// Mode is "pod" in a config file.
	Mode string `json:"mode,omitempty"`
	Name string `json:"name"`
	// WebAddr is the address of the web page; empty disables it.
	WebAddr string `json:"webAddr"`
	// WebAuth, "user:password", protects the web page.
	WebAuth string `json:"webAuth,omitempty"`
	// Servers are the fibhub servers to connect to.
	Servers []uplink.Config `json:"servers"`
	// Nodes are the nodes the pod can reach, and the only ones it connects
	// to.
	Nodes []proto.Node `json:"nodes"`
	// Meta is told to the servers with the pod's info.
	Meta map[string]string `json:"meta,omitempty"`
}

// DialTimeout bounds a pod's connect to a node. It is shorter than the time
// a server waits for the pod's answer, so that the server hears either way.
var DialTimeout = 5 * time.Second

// Pod is a running pod.
type Pod struct {
	engine     *fib.Engine
	configPath string
	out        io.Writer
	// webAddr is the address the web page listens on, as bound.
	webAddr string

	mu      sync.Mutex
	cfg     Config
	uplinks []*uplink.Uplink
	// nodeRTT measures, by node name, the connects to the nodes.
	nodeRTT map[string]*latency.Meter
}

// Run runs a pod with cfg until ctx ends, saving changes made on its web
// page to configPath, unless that is empty. ready, if not nil, is told the
// pod once it is running.
func Run(ctx context.Context, cfg Config, configPath string, out io.Writer, ready func(*Pod)) error {
	if cfg.Name == "" {
		return errors.New("pod: a name is required")
	}
	if err := ValidateNodes(cfg.Nodes); err != nil {
		return err
	}
	if cfg.Nodes == nil {
		cfg.Nodes = []proto.Node{}
	}
	cfg.Mode = "pod"
	for i := range cfg.Servers {
		if cfg.Servers[i].Name == "" {
			cfg.Servers[i].Name = cfg.Servers[i].Addr
		}
	}
	p := &Pod{configPath: configPath, out: out, cfg: cfg, nodeRTT: map[string]*latency.Meter{}}
	// The file keeps what the flags added to it, too.
	p.saveLocked()
	engine, err := fib.New(fib.DefaultConfig())
	if err != nil {
		return err
	}
	p.engine = engine
	webAddr := ""
	if cfg.WebAddr != "" {
		i, err := engine.AddBinding(fib.Binding{Addr: cfg.WebAddr, Handler: p.webHandler()})
		if err != nil {
			_ = engine.Close()
			return err
		}
		if addrs, err := engine.BindingAddrs(i); err == nil && len(addrs) > 0 {
			webAddr = addrs[0].String()
		}
	}
	p.webAddr = webAddr
	running := make(chan error, 1)
	go func() { running <- engine.Run() }()

	for _, sc := range cfg.Servers {
		p.startUplink(sc)
	}
	fmt.Fprintf(out, "fibhub pod %q: %d servers, %d nodes", cfg.Name, len(cfg.Servers), len(cfg.Nodes))
	if webAddr != "" {
		fmt.Fprintf(out, ", web on http://%s", webAddr)
	}
	fmt.Fprintln(out)
	if ready != nil {
		ready(p)
	}

	select {
	case <-ctx.Done():
	case err = <-running:
	}
	p.mu.Lock()
	uplinks := p.uplinks
	p.uplinks = nil
	p.mu.Unlock()
	for _, u := range uplinks {
		u.Stop()
	}
	engine.Stop()
	if err == nil {
		err = <-running
	}
	_ = engine.Close()
	return err
}

// ValidateNodes checks that nodes have names of their own and TCP addresses.
func ValidateNodes(nodes []proto.Node) error {
	seen := map[string]bool{}
	for _, n := range nodes {
		if n.Name == "" {
			return errors.New("pod: a node needs a name")
		}
		if seen[n.Name] {
			return fmt.Errorf("pod: two nodes named %q", n.Name)
		}
		seen[n.Name] = true
		if _, port, err := net.SplitHostPort(n.Addr); err != nil || port == "" {
			return fmt.Errorf("pod: node %q: address %q is not host:port", n.Name, n.Addr)
		}
	}
	return nil
}

// Info is what the pod tells servers about itself.
func (p *Pod) Info() proto.Info {
	p.mu.Lock()
	defer p.mu.Unlock()
	info := proto.NewInfo(p.cfg.Name, proto.TypePod)
	info.Nodes = append([]proto.Node{}, p.cfg.Nodes...)
	info.Meta = p.cfg.Meta
	return info
}

// Config is the pod's settings.
func (p *Pod) Config() Config {
	p.mu.Lock()
	defer p.mu.Unlock()
	cfg := p.cfg
	cfg.Servers = append([]uplink.Config(nil), cfg.Servers...)
	cfg.Nodes = append([]proto.Node{}, cfg.Nodes...)
	return cfg
}

func (p *Pod) saveLocked() {
	if err := common.SaveJSON(p.configPath, p.cfg); err != nil {
		slog.Error("fibhub: saving the config", "path", p.configPath, "err", err)
	}
}

// NodeRTT is, by node name, how long the connects to the nodes took. A
// node the pod has not connected to is missing.
func (p *Pod) NodeRTT() map[string]latency.Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]latency.Stats, len(p.nodeRTT))
	for name, m := range p.nodeRTT {
		out[name] = m.Stats()
	}
	return out
}

// nodeMeter is the meter of the connects to the node named name.
func (p *Pod) nodeMeter(name string) *latency.Meter {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.nodeRTT[name]
	if m == nil {
		m = &latency.Meter{}
		p.nodeRTT[name] = m
	}
	return m
}

func (p *Pod) startUplink(sc uplink.Config) *uplink.Uplink {
	u := uplink.New(p.engine, sc, p.Info, p.setupUplink)
	// Servers tell clients how fast the pod reaches its nodes.
	u.OnPing = func(req *proto.PingReq) { req.NodeRTT = p.NodeRTT() }
	p.mu.Lock()
	p.uplinks = append(p.uplinks, u)
	p.mu.Unlock()
	u.Start()
	return u
}

// setupUplink registers the methods a server calls on the pod.
func (p *Pod) setupUplink(u *uplink.Uplink, h *arpc.Handler) {
	h.Handle(proto.MethodPodInfo, func(ctx *arpc.Context) {
		info := p.Info()
		_ = ctx.Write(&info)
	})
	h.Handle(proto.MethodPodSetNodes, func(ctx *arpc.Context) {
		var nodes []proto.Node
		if err := ctx.Bind(&nodes); err != nil {
			_ = ctx.Error(err)
			return
		}
		if err := p.SetNodes(nodes, u); err != nil {
			_ = ctx.Error(err)
			return
		}
		info := p.Info()
		_ = ctx.Write(&info)
	})
	h.Handle(proto.MethodTunnelOpen, func(ctx *arpc.Context) {
		var req proto.TunnelOpenReq
		if err := ctx.Bind(&req); err != nil {
			_ = ctx.Error(err)
			return
		}
		rsp, err := p.openTunnel(u, ctx.Client, &req)
		if err != nil {
			slog.Warn("fibhub: tunnel open failed", "id", req.ID, "node", req.Node, "client", req.Client, "err", err)
			_ = ctx.Error(err)
			return
		}
		_ = ctx.Write(rsp)
	})
}

// openTunnel connects tunnel req.ID to the node it names.
func (p *Pod) openTunnel(u *uplink.Uplink, link *arpc.Client, req *proto.TunnelOpenReq) (*proto.TunnelOpenRsp, error) {
	info := p.Info()
	node, ok := info.FindNode(req.Node)
	if !ok {
		return nil, fmt.Errorf("pod: no node %q", req.Node)
	}
	if u.Tunnels.Get(req.ID) != nil {
		return nil, errors.New("pod: tunnel ID in use")
	}
	meter := p.nodeMeter(node.Name)
	start := time.Now()
	handler := &tunnel.Handler{Open: func(c *fib.Connection) *tunnel.Endpoint {
		d := time.Since(start)
		meter.Add(d)
		e := tunnel.New(u.Tunnels, link, req.ID, true)
		e.Pod, e.Node, e.Addr, e.Peer = info.Name, node.Name, node.Addr, req.Client
		e.ConnectRTT = latency.Ms(d)
		if !u.Tunnels.Add(e) {
			return nil
		}
		return e
	}}
	dialed := make(chan error, 1)
	err := p.engine.DialWithHandler("tcp", node.Addr, DialTimeout, handler, func(c *fib.Connection, err error) {
		if err == nil && c.Attachment() == nil {
			err = errors.New("pod: tunnel ID in use")
		}
		dialed <- err
	})
	if err == nil {
		err = <-dialed
	}
	if err != nil {
		meter.Lost()
		return nil, err
	}
	slog.Info("fibhub: tunnel open", "id", req.ID, "client", req.Client, "node", node.Name, "addr", node.Addr)
	return &proto.TunnelOpenRsp{ID: req.ID, Addr: node.Addr}, nil
}

// SetNodes replaces the nodes the pod can reach, saves them and tells every
// server but from, which asked for the change and hears of it in the answer.
func (p *Pod) SetNodes(nodes []proto.Node, from *uplink.Uplink) error {
	if err := ValidateNodes(nodes); err != nil {
		return err
	}
	p.mu.Lock()
	// What was measured of a node goes with it, and a node whose address
	// changes is measured anew.
	was, will := proto.Info{Nodes: p.cfg.Nodes}, proto.Info{Nodes: nodes}
	for name := range p.nodeRTT {
		o, _ := was.FindNode(name)
		if n, ok := will.FindNode(name); !ok || n.Addr != o.Addr {
			delete(p.nodeRTT, name)
		}
	}
	p.cfg.Nodes = append([]proto.Node{}, nodes...)
	p.saveLocked()
	uplinks := append([]*uplink.Uplink(nil), p.uplinks...)
	p.mu.Unlock()
	info := p.Info()
	for _, u := range uplinks {
		if u == from {
			continue
		}
		go func() {
			if err := u.Call(proto.MethodUpdate, &info, nil); err != nil && !errors.Is(err, uplink.ErrNotRegistered) {
				slog.Warn("fibhub: updating the server", "server", u.Config.Addr, "err", err)
			}
		}()
	}
	return nil
}

// AddServer connects to another server and saves it.
func (p *Pod) AddServer(sc uplink.Config) error {
	if sc.Addr == "" {
		return errors.New("pod: a server needs an address")
	}
	if sc.Name == "" {
		sc.Name = sc.Addr
	}
	p.mu.Lock()
	for _, s := range p.cfg.Servers {
		if s.Name == sc.Name {
			p.mu.Unlock()
			return fmt.Errorf("pod: a server named %q exists", sc.Name)
		}
	}
	p.cfg.Servers = append(p.cfg.Servers, sc)
	p.saveLocked()
	p.mu.Unlock()
	p.startUplink(sc)
	return nil
}

// RemoveServer disconnects from the server named name and forgets it.
func (p *Pod) RemoveServer(name string) error {
	p.mu.Lock()
	var found *uplink.Uplink
	for i, u := range p.uplinks {
		if u.Config.Name == name {
			found = u
			p.uplinks = append(p.uplinks[:i:i], p.uplinks[i+1:]...)
			break
		}
	}
	for i, s := range p.cfg.Servers {
		if s.Name == name {
			p.cfg.Servers = append(p.cfg.Servers[:i:i], p.cfg.Servers[i+1:]...)
			break
		}
	}
	p.saveLocked()
	p.mu.Unlock()
	if found == nil {
		return fmt.Errorf("pod: no server named %q", name)
	}
	found.Stop()
	return nil
}

// Uplinks is the pod's connections to servers.
func (p *Pod) Uplinks() []*uplink.Uplink {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*uplink.Uplink(nil), p.uplinks...)
}

func (p *Pod) uplink(name string) *uplink.Uplink {
	for _, u := range p.Uplinks() {
		if u.Config.Name == name {
			return u
		}
	}
	return nil
}

// TunnelView is a tunnel as the web page shows it.
type TunnelView struct {
	Server string `json:"server"`
	tunnel.Snapshot
}

// Tunnels is every open tunnel.
func (p *Pod) Tunnels() []TunnelView {
	out := []TunnelView{}
	for _, u := range p.Uplinks() {
		for _, s := range u.Tunnels.List() {
			out = append(out, TunnelView{Server: u.Config.Name, Snapshot: s})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}
