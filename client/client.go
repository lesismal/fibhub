//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Package client is fibhub's client mode. A client connects to one or more
// fibhub servers and learns the pods they have and the nodes those pods
// reach. For each node it is told to forward, it listens on a local TCP
// address; every connection it accepts there becomes a tunnel, which the
// server routes to the node's pod and the pod connects to the node. A web
// page shows the servers, pods, forwards and tunnels and configures them; a
// client given a config file saves those settings there.
package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib/arpc"
	"github.com/lesismal/fibhub/internal/common"
	"github.com/lesismal/fibhub/internal/proto"
	"github.com/lesismal/fibhub/internal/tunnel"
	"github.com/lesismal/fibhub/internal/uplink"
)

// OpenTimeout bounds the opening of a tunnel.
var OpenTimeout = 20 * time.Second

// Forward is a local address whose connections are tunneled to a node.
type Forward struct {
	// Listen is the local address, host:port.
	Listen string `json:"listen"`
	// Server names the server to go through; empty picks the first one that
	// has the pod.
	Server string `json:"server,omitempty"`
	Pod    string `json:"pod"`
	Node   string `json:"node"`
	// WebAuth, for a node that is a web page, such as a pod's, is its
	// user:password, which the client's web page shows and opens it with.
	WebAuth string `json:"webAuth,omitempty"`
}

// Config is a client's settings, as its config file holds them.
type Config struct {
	// Mode is "client" in a config file.
	Mode string `json:"mode,omitempty"`
	Name string `json:"name"`
	// WebAddr is the address of the web page; empty disables it.
	WebAddr string `json:"webAddr"`
	// WebAuth, "user:password", protects the web page.
	WebAuth  string          `json:"webAuth,omitempty"`
	Servers  []uplink.Config `json:"servers"`
	Forwards []Forward       `json:"forwards"`
	// WebProxies reach servers' web pages through their encrypted
	// WebSockets.
	WebProxies []WebProxy `json:"webProxies,omitempty"`
	// Meta is told to the servers with the client's info.
	Meta map[string]string `json:"meta,omitempty"`
}

// forward is a Forward and its listener.
type forward struct {
	Forward
	binding int
	addr    string
	err     string
}

// Client is a running client.
type Client struct {
	engine     *fib.Engine
	configPath string
	out        io.Writer

	mu       sync.Mutex
	cfg      Config
	uplinks  []*uplink.Uplink
	pods     map[*uplink.Uplink]*proto.PodList
	forwards []*forward
	proxies  []string
}

// Run runs a client with cfg until ctx ends, saving changes made on its web
// page to configPath, unless that is empty. ready, if not nil, is told the
// client once it is running.
func Run(ctx context.Context, cfg Config, configPath string, out io.Writer, ready func(*Client)) error {
	if cfg.Name == "" {
		return errors.New("client: a name is required")
	}
	cfg.Mode = "client"
	for i := range cfg.Servers {
		if cfg.Servers[i].Name == "" {
			cfg.Servers[i].Name = cfg.Servers[i].Addr
		}
	}
	c := &Client{configPath: configPath, out: out, cfg: cfg, pods: map[*uplink.Uplink]*proto.PodList{}}
	engine, err := fib.New(fib.DefaultConfig())
	if err != nil {
		return err
	}
	c.engine = engine
	webAddr := ""
	if cfg.WebAddr != "" {
		i, err := engine.AddBinding(fib.Binding{Addr: cfg.WebAddr, Handler: c.webHandler()})
		if err != nil {
			_ = engine.Close()
			return err
		}
		if addrs, err := engine.BindingAddrs(i); err == nil && len(addrs) > 0 {
			webAddr = addrs[0].String()
		}
	}
	for _, wp := range cfg.WebProxies {
		addr, err := c.startWebProxy(wp)
		if err != nil {
			_ = engine.Close()
			return err
		}
		c.proxies = append(c.proxies, addr)
		fmt.Fprintf(out, "fibhub client: web page of %s on http://%s\n", wp.URL, addr)
	}
	running := make(chan error, 1)
	go func() { running <- engine.Run() }()

	for _, sc := range cfg.Servers {
		c.startUplink(sc)
	}
	for _, f := range cfg.Forwards {
		fw := c.listen(f)
		c.mu.Lock()
		c.forwards = append(c.forwards, fw)
		c.mu.Unlock()
		if fw.err != "" {
			fmt.Fprintf(out, "fibhub client: forward %s: %s\n", f.Listen, fw.err)
		}
	}
	fmt.Fprintf(out, "fibhub client %q: %d servers, %d forwards", cfg.Name, len(cfg.Servers), len(cfg.Forwards))
	if webAddr != "" {
		fmt.Fprintf(out, ", web on http://%s", webAddr)
	}
	fmt.Fprintln(out)
	// The file keeps what the flags added to it, too.
	c.mu.Lock()
	c.saveLocked()
	c.mu.Unlock()
	if ready != nil {
		ready(c)
	}

	select {
	case <-ctx.Done():
	case err = <-running:
	}
	c.mu.Lock()
	uplinks := c.uplinks
	c.uplinks = nil
	c.mu.Unlock()
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

// WebProxyView is a web proxy as the web page shows it.
type WebProxyView struct {
	Name    string `json:"name,omitempty"`
	Listen  string `json:"listen"`
	Addr    string `json:"addr"`
	URL     string `json:"url"`
	WebAuth string `json:"webAuth,omitempty"`
}

// WebProxies is the client's web proxies.
func (c *Client) WebProxies() []WebProxyView {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]WebProxyView, len(c.proxies))
	for i, addr := range c.proxies {
		wp := c.cfg.WebProxies[i]
		out[i] = WebProxyView{Name: wp.Name, Listen: wp.Listen, Addr: addr, URL: wp.URL, WebAuth: wp.WebAuth}
	}
	return out
}

// SetWebAuth sets the web page login of the web proxy, or else the forward,
// that listens on listen, and saves it.
func (c *Client) SetWebAuth(listen, auth string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.cfg.WebProxies {
		if c.cfg.WebProxies[i].Listen == listen {
			c.cfg.WebProxies[i].WebAuth = auth
			c.saveLocked()
			return nil
		}
	}
	for _, fw := range c.forwards {
		if fw.Listen == listen {
			fw.WebAuth = auth
			c.saveLocked()
			return nil
		}
	}
	return fmt.Errorf("client: no web proxy or forward on %s", listen)
}

// WebProxyAddrs is the addresses the web proxies listen on.
func (c *Client) WebProxyAddrs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.proxies...)
}

// Info is what the client tells servers about itself.
func (c *Client) Info() proto.Info {
	c.mu.Lock()
	defer c.mu.Unlock()
	info := proto.NewInfo(c.cfg.Name, proto.TypeClient)
	info.Meta = c.cfg.Meta
	return info
}

func (c *Client) saveLocked() {
	cfg := c.cfg
	cfg.Forwards = make([]Forward, len(c.forwards))
	for i, f := range c.forwards {
		cfg.Forwards[i] = f.Forward
	}
	if err := common.SaveJSON(c.configPath, cfg); err != nil {
		slog.Error("fibhub: saving the config", "path", c.configPath, "err", err)
	}
}

func (c *Client) startUplink(sc uplink.Config) *uplink.Uplink {
	u := uplink.New(c.engine, sc, c.Info, func(u *uplink.Uplink, h *arpc.Handler) {
		// The server sends the pods whenever they change.
		h.Handle(proto.MethodPods, func(ctx *arpc.Context) {
			var list proto.PodList
			if err := ctx.Bind(&list); err == nil {
				c.setPods(u, &list)
			}
		}, false)
	})
	u.OnRegistered = func(u *uplink.Uplink) {
		if _, err := c.RefreshPods(u); err != nil {
			slog.Warn("fibhub: listing pods", "server", u.Config.Addr, "err", err)
		}
	}
	c.mu.Lock()
	c.uplinks = append(c.uplinks, u)
	c.mu.Unlock()
	u.Start()
	return u
}

func (c *Client) setPods(u *uplink.Uplink, list *proto.PodList) {
	c.mu.Lock()
	c.pods[u] = list
	c.mu.Unlock()
}

// RefreshPods asks the server of u for its pods.
func (c *Client) RefreshPods(u *uplink.Uplink) (*proto.PodList, error) {
	var list proto.PodList
	if err := u.Call(proto.MethodPods, nil, &list); err != nil {
		return nil, err
	}
	c.setPods(u, &list)
	return &list, nil
}

// Pods is what the server of u last said its pods were.
func (c *Client) Pods(u *uplink.Uplink) *proto.PodList {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pods[u]
}

// SetPodNodes has the server of u set the nodes of pod.
func (c *Client) SetPodNodes(u *uplink.Uplink, pod string, nodes []proto.Node) (proto.Info, error) {
	var info proto.Info
	if nodes == nil {
		nodes = []proto.Node{}
	}
	err := u.Call(proto.MethodSetPodNodes, &proto.SetNodesReq{Pod: pod, Nodes: nodes}, &info)
	if err == nil {
		_, _ = c.RefreshPods(u)
	}
	return info, err
}

// SetMeta changes what the client tells servers about itself, and tells
// them.
func (c *Client) SetMeta(meta map[string]string) {
	c.mu.Lock()
	c.cfg.Meta = meta
	c.saveLocked()
	uplinks := append([]*uplink.Uplink(nil), c.uplinks...)
	c.mu.Unlock()
	info := c.Info()
	for _, u := range uplinks {
		go func() {
			if err := u.Call(proto.MethodUpdate, &info, nil); err != nil && !errors.Is(err, uplink.ErrNotRegistered) {
				slog.Warn("fibhub: updating the server", "server", u.Config.Addr, "err", err)
			}
		}()
	}
}

// Uplinks is the client's connections to servers.
func (c *Client) Uplinks() []*uplink.Uplink {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*uplink.Uplink(nil), c.uplinks...)
}

func (c *Client) uplink(name string) *uplink.Uplink {
	for _, u := range c.Uplinks() {
		if u.Config.Name == name {
			return u
		}
	}
	return nil
}

// AddServer connects to another server and saves it.
func (c *Client) AddServer(sc uplink.Config) error {
	if sc.Addr == "" {
		return errors.New("client: a server needs an address")
	}
	if sc.Name == "" {
		sc.Name = sc.Addr
	}
	c.mu.Lock()
	for _, s := range c.cfg.Servers {
		if s.Name == sc.Name {
			c.mu.Unlock()
			return fmt.Errorf("client: a server named %q exists", sc.Name)
		}
	}
	c.cfg.Servers = append(c.cfg.Servers, sc)
	c.saveLocked()
	c.mu.Unlock()
	c.startUplink(sc)
	return nil
}

// RemoveServer disconnects from the server named name and forgets it.
func (c *Client) RemoveServer(name string) error {
	c.mu.Lock()
	var found *uplink.Uplink
	for i, u := range c.uplinks {
		if u.Config.Name == name {
			found = u
			c.uplinks = append(c.uplinks[:i:i], c.uplinks[i+1:]...)
			delete(c.pods, u)
			break
		}
	}
	for i, s := range c.cfg.Servers {
		if s.Name == name {
			c.cfg.Servers = append(c.cfg.Servers[:i:i], c.cfg.Servers[i+1:]...)
			break
		}
	}
	c.saveLocked()
	c.mu.Unlock()
	if found == nil {
		return fmt.Errorf("client: no server named %q", name)
	}
	found.Stop()
	return nil
}

// pick is the uplink a tunnel of f goes through: the server f names, or else
// the first registered one that lists f's pod with f's node.
func (c *Client) pick(f Forward) (*uplink.Uplink, error) {
	if f.Server != "" {
		u := c.uplink(f.Server)
		if u == nil {
			return nil, fmt.Errorf("no server named %q", f.Server)
		}
		return u, nil
	}
	for _, u := range c.Uplinks() {
		if _, err := u.Client(); err != nil {
			continue
		}
		list := c.Pods(u)
		if list == nil {
			continue
		}
		for _, p := range list.Pods {
			if p.Info.Name == f.Pod {
				if _, ok := p.Info.FindNode(f.Node); ok {
					return u, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("no server has pod %q with node %q", f.Pod, f.Node)
}

// listen opens f's listener.
func (c *Client) listen(f Forward) *forward {
	fw := &forward{Forward: f, binding: -1}
	handler := &tunnel.Handler{
		Open: func(conn *fib.Connection) *tunnel.Endpoint {
			u, err := c.pick(f)
			if err != nil {
				slog.Warn("fibhub: forward refused a connection", "listen", f.Listen, "err", err)
				return nil
			}
			id, link, err := u.NextTunnelID()
			if err != nil {
				slog.Warn("fibhub: forward refused a connection", "listen", f.Listen, "server", u.Config.Name, "err", err)
				return nil
			}
			e := tunnel.New(u.Tunnels, link, id, false)
			e.Pod, e.Node, e.Peer = f.Pod, f.Node, f.Listen
			if !u.Tunnels.Add(e) {
				return nil
			}
			return e
		},
		Opened: func(e *tunnel.Endpoint) {
			go c.open(e, f)
		},
	}
	i, err := c.engine.AddBinding(fib.Binding{Addr: f.Listen, Handler: handler})
	if err != nil {
		fw.err = err.Error()
		return fw
	}
	fw.binding = i
	if addrs, err := c.engine.BindingAddrs(i); err == nil && len(addrs) > 0 {
		fw.addr = addrs[0].String()
	}
	slog.Info("fibhub: forwarding", "listen", fw.addr, "pod", f.Pod, "node", f.Node)
	return fw
}

// open asks the server to open e's tunnel, and lets e read its connection
// once it is open.
func (c *Client) open(e *tunnel.Endpoint, f Forward) {
	var rsp proto.TunnelOpenRsp
	req := proto.TunnelOpenReq{ID: e.ID, Pod: f.Pod, Node: f.Node}
	link, ok := e.Link().(*arpc.Client)
	if !ok {
		e.Close("no link")
		return
	}
	if err := link.Call(proto.MethodTunnelOpen, &req, &rsp, OpenTimeout); err != nil {
		slog.Warn("fibhub: tunnel open failed", "id", e.ID, "pod", f.Pod, "node", f.Node, "err", err)
		e.Close("open failed: " + err.Error())
		return
	}
	e.SetAddr(rsp.Addr)
	e.SetReady()
	slog.Info("fibhub: tunnel open", "id", e.ID, "pod", f.Pod, "node", f.Node, "addr", rsp.Addr)
}

// AddForward listens for f and saves it.
func (c *Client) AddForward(f Forward) error {
	if f.Listen == "" || f.Pod == "" || f.Node == "" {
		return errors.New("client: a forward needs a listen address, a pod and a node")
	}
	c.mu.Lock()
	for _, fw := range c.forwards {
		if fw.Listen == f.Listen {
			c.mu.Unlock()
			return fmt.Errorf("client: %s is forwarded already", f.Listen)
		}
	}
	c.mu.Unlock()
	fw := c.listen(f)
	if fw.err != "" {
		return errors.New(fw.err)
	}
	c.mu.Lock()
	c.forwards = append(c.forwards, fw)
	c.saveLocked()
	c.mu.Unlock()
	return nil
}

// RemoveForward stops listening on listen and forgets it. Its open tunnels
// stay open.
func (c *Client) RemoveForward(listen string) error {
	c.mu.Lock()
	var found *forward
	for i, fw := range c.forwards {
		if fw.Listen == listen {
			found = fw
			c.forwards = append(c.forwards[:i:i], c.forwards[i+1:]...)
			break
		}
	}
	if found != nil {
		c.saveLocked()
	}
	c.mu.Unlock()
	if found == nil {
		return fmt.Errorf("client: %s is not forwarded", listen)
	}
	if found.binding >= 0 {
		return c.engine.RemoveBinding(found.binding)
	}
	return nil
}

// ForwardView is a forward as the web page shows it.
type ForwardView struct {
	Forward
	Addr  string `json:"addr,omitempty"`
	Error string `json:"error,omitempty"`
}

// Forwards is the client's forwards.
func (c *Client) Forwards() []ForwardView {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ForwardView, len(c.forwards))
	for i, fw := range c.forwards {
		out[i] = ForwardView{Forward: fw.Forward, Addr: fw.addr, Error: fw.err}
	}
	return out
}

// TunnelView is a tunnel as the web page shows it.
type TunnelView struct {
	Server string `json:"server"`
	tunnel.Snapshot
}

// Tunnels is every open tunnel.
func (c *Client) Tunnels() []TunnelView {
	out := []TunnelView{}
	for _, u := range c.Uplinks() {
		for _, s := range u.Tunnels.List() {
			out = append(out, TunnelView{Server: u.Config.Name, Snapshot: s})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}
