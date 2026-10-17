//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	stdhttp "net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lesismal/fibhub/client"
	"github.com/lesismal/fibhub/internal/proto"
	"github.com/lesismal/fibhub/internal/uplink"
	"github.com/lesismal/fibhub/pod"
	"github.com/lesismal/fibhub/server"
)

const password = "s3cret"

// echoNode is a node that echoes what it reads; with a banner it says that
// first.
func echoNode(t *testing.T, banner string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				if banner != "" {
					io.WriteString(c, banner)
				}
				io.Copy(c, c)
			}()
		}
	}()
	return l.Addr().String()
}

// stallNode is a node that reads nothing until the test takes the
// connection it accepted.
func stallNode(t *testing.T) (<-chan net.Conn, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	conns := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			conns <- c
		}
	}()
	return conns, l.Addr().String()
}

// start runs a mode until the test ends, and returns what it was ready with.
func start[T any](t *testing.T, run func(ctx context.Context, ready func(T)) error) T {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	readyc := make(chan T, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, func(v T) { readyc <- v }) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("run did not return")
		}
	})
	select {
	case v := <-readyc:
		return v
	case err := <-done:
		t.Fatalf("run: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("not ready")
	}
	panic("unreachable")
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type hub struct {
	server *server.Server
	pod    *pod.Pod
	client *client.Client
}

// hubOpts are how a hub's peers reach its server.
type hubOpts struct {
	// ws has pods and clients reach arpc over WebSocket.
	ws bool
	// body, when set, encrypts arpc bodies.
	body    string
	proxies bool
}

func startHub(t *testing.T, nodes []proto.Node, forwards []client.Forward, opts hubOpts) *hub {
	t.Helper()
	h := &hub{}
	h.server = start(t, func(ctx context.Context, ready func(*server.Server)) error {
		return server.Run(ctx, server.Config{
			Name: "hub", Addr: "127.0.0.1:0", WebAddr: "127.0.0.1:0", WSAddr: "127.0.0.1:0", ArpcWSAddr: "127.0.0.1:0",
			Password: password, BodyPassword: opts.body,
		}, io.Discard, ready)
	})
	addr := h.server.Addr()
	if opts.ws {
		addr = h.server.ArpcWSURL()
	}
	servers := []uplink.Config{{Name: "hub", Addr: addr, Password: password, BodyPassword: opts.body}}
	h.pod = start(t, func(ctx context.Context, ready func(*pod.Pod)) error {
		return pod.Run(ctx, pod.Config{Name: "lab", Servers: servers, Nodes: nodes}, "", io.Discard, ready)
	})
	cfg := client.Config{Name: "me", Servers: servers, Forwards: forwards}
	if opts.proxies {
		cfg.WebProxies = []client.WebProxy{{Listen: "127.0.0.1:0", URL: "ws://" + h.server.WSAddr() + "/", Password: password}}
	}
	h.client = start(t, func(ctx context.Context, ready func(*client.Client)) error {
		return client.Run(ctx, cfg, "", io.Discard, ready)
	})
	waitFor(t, "the client to see the pod", func() bool {
		for _, s := range h.client.Status().Servers {
			for _, p := range s.Pods {
				if p.Info.Name == "lab" && len(p.Info.Nodes) == len(nodes) {
					return true
				}
			}
		}
		return false
	})
	return h
}

func (h *hub) forwardAddr(t *testing.T, listen string) string {
	for _, f := range h.client.Forwards() {
		if f.Listen == listen {
			if f.Error != "" {
				t.Fatalf("forward %s: %s", listen, f.Error)
			}
			return f.Addr
		}
	}
	t.Fatalf("no forward %s", listen)
	return ""
}

// TestTunnel runs the tunnel tests over each way peers can reach the
// server.
func TestTunnel(t *testing.T) {
	for _, opts := range []hubOpts{{}, {ws: true}, {body: "aes-key"}, {ws: true, body: "aes-key"}} {
		name := "tcp"
		if opts.ws {
			name = "websocket"
		}
		if opts.body != "" {
			name += "+aes"
		}
		t.Run(name, func(t *testing.T) { testTunnel(t, opts) })
	}
}

func testTunnel(t *testing.T, opts hubOpts) {
	stalled, stallAddr := stallNode(t)
	nodes := []proto.Node{
		{Name: "echo", Addr: echoNode(t, "")},
		{Name: "banner", Addr: echoNode(t, "hello from the node\n")},
		{Name: "stall", Addr: stallAddr},
	}
	stallListen := freeAddr(t)
	forwards := []client.Forward{
		{Listen: "127.0.0.1:0", Pod: "lab", Node: "echo"},
		{Listen: stallListen, Pod: "lab", Node: "stall"},
	}
	h := startHub(t, nodes, forwards, opts)
	addr := h.forwardAddr(t, "127.0.0.1:0")

	t.Run("echo", func(t *testing.T) {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.WriteString(c, "ping"); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 4)
		if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
			t.Fatalf("%q, %v", buf, err)
		}
	})

	// Far more than a tunnel's window goes each way at once, while the
	// writer does not read, so the windows have to pace it.
	t.Run("bulk", func(t *testing.T) {
		const size = 48 << 20
		data := make([]byte, size)
		rand.Read(data)
		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c, err := net.Dial("tcp", addr)
				if err != nil {
					t.Error(err)
					return
				}
				defer c.Close()
				c.SetDeadline(time.Now().Add(60 * time.Second))
				go func() {
					if _, err := c.Write(data); err != nil {
						t.Error(err)
					}
				}()
				got := make([]byte, size)
				if _, err := io.ReadFull(c, got); err != nil {
					t.Error(err)
					return
				}
				if !bytes.Equal(got, data) {
					t.Error("echo differs")
				}
			}()
		}
		wg.Wait()
	})

	// A node that does not read holds back the client that writes to it,
	// with no more than a window and the sockets' buffers between them.
	t.Run("backpressure", func(t *testing.T) {
		c, err := net.Dial("tcp", stallListen)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		chunk := make([]byte, 64<<10)
		var written int
		c.SetWriteDeadline(time.Now().Add(2 * time.Second))
		for written < 256<<20 {
			n, err := c.Write(chunk)
			written += n
			if err != nil {
				break
			}
		}
		if written >= 256<<20 {
			t.Fatalf("wrote %d MiB to a node that reads nothing", written>>20)
		}
		t.Logf("held back after %.1f MiB", float64(written)/(1<<20))
		// Once the node reads, everything arrives.
		node := <-stalled
		defer node.Close()
		c.SetWriteDeadline(time.Time{})
		node.SetReadDeadline(time.Now().Add(20 * time.Second))
		if n, err := io.CopyN(io.Discard, node, int64(written)); err != nil {
			t.Fatalf("node read %d of %d: %v", n, written, err)
		}
	})

	bannerListen := freeAddr(t)
	t.Run("forward added later", func(t *testing.T) {
		if err := h.client.AddForward(client.Forward{Listen: bannerListen, Pod: "nope", Node: "x"}); err != nil {
			t.Fatal(err)
		}
		if err := h.client.AddForward(client.Forward{Listen: bannerListen, Pod: "lab", Node: "banner", Server: "hub"}); err == nil {
			t.Fatal("two forwards on one listen address")
		}
		// A connection to a pod nobody has is closed.
		c, err := net.Dial("tcp", bannerListen)
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(10 * time.Second))
		if n, err := c.Read(make([]byte, 1)); err == nil {
			t.Fatalf("read %d from a forward to no pod", n)
		}
		c.Close()
		if err := h.client.RemoveForward(bannerListen); err != nil {
			t.Fatal(err)
		}
		if err := h.client.AddForward(client.Forward{Listen: bannerListen, Pod: "lab", Node: "banner", Server: "hub"}); err != nil {
			t.Fatal(err)
		}
		c, err = net.Dial("tcp", bannerListen)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(10 * time.Second))
		want := "hello from the node\n"
		buf := make([]byte, len(want))
		if _, err := io.ReadFull(c, buf); err != nil || string(buf) != want {
			t.Fatalf("%q, %v", buf, err)
		}
	})

	t.Run("server closes a tunnel", func(t *testing.T) {
		c, err := net.Dial("tcp", bannerListen)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(10 * time.Second))
		buf := make([]byte, 20)
		if _, err := io.ReadFull(c, buf); err != nil {
			t.Fatal(err)
		}
		var id uint64
		waitFor(t, "the tunnel at the server", func() bool {
			for _, r := range h.server.Status().Tunnels {
				if r.Node == "banner" {
					id = r.ID
					return true
				}
			}
			return false
		})
		web := "http://" + h.server.WebAddr()
		rsp, err := stdhttp.Post(fmt.Sprintf("%s/api/tunnels/%d/close", web, id), "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		rsp.Body.Close()
		if rsp.StatusCode != 200 {
			t.Fatalf("close: %s", rsp.Status)
		}
		if n, err := c.Read(buf); err != io.EOF {
			t.Fatalf("read %d, %v after the server closed the tunnel", n, err)
		}
		waitFor(t, "the pod to close its end", func() bool {
			for _, tv := range h.pod.Tunnels() {
				if tv.ID == id {
					return false
				}
			}
			return true
		})
	})

	t.Run("a closed client closes the node", func(t *testing.T) {
		before := len(h.pod.Tunnels())
		c, err := net.Dial("tcp", bannerListen)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 20)
		c.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.ReadFull(c, buf); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the pod's end", func() bool { return len(h.pod.Tunnels()) == before+1 })
		c.Close()
		waitFor(t, "the pod's end to close", func() bool { return len(h.pod.Tunnels()) == before })
		waitFor(t, "the server's route to go", func() bool { return len(h.server.Status().Tunnels) == 0 })
	})

	t.Run("server updates the pod's nodes", func(t *testing.T) {
		web := "http://" + h.server.WebAddr()
		var podID uint32
		for _, p := range h.server.Status().Peers {
			if p.Info.Type == proto.TypePod {
				podID = p.ID
			}
		}
		nodes := append(append([]proto.Node{}, nodes...), proto.Node{Name: "more", Addr: nodes[0].Addr})
		body, _ := json.Marshal(nodes)
		rsp, err := stdhttp.Post(fmt.Sprintf("%s/api/peers/%d/nodes", web, podID), "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rsp.Body)
		rsp.Body.Close()
		if rsp.StatusCode != 200 {
			t.Fatalf("set nodes: %s %s", rsp.Status, b)
		}
		if got := h.pod.Config().Nodes; len(got) != len(nodes) {
			t.Fatalf("pod nodes %v", got)
		}
		waitFor(t, "the client to hear of the node", func() bool {
			for _, s := range h.client.Status().Servers {
				for _, p := range s.Pods {
					if _, ok := p.Info.FindNode("more"); ok {
						return true
					}
				}
			}
			return false
		})
		// A node that is not host:port is refused.
		body, _ = json.Marshal([]proto.Node{{Name: "bad", Addr: "nowhere"}})
		rsp, err = stdhttp.Post(fmt.Sprintf("%s/api/peers/%d/nodes", web, podID), "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		rsp.Body.Close()
		if rsp.StatusCode == 200 {
			t.Fatal("accepted a bad node")
		}
	})
}

// freeAddr is a local address nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestWebProxy(t *testing.T) {
	h := startHub(t, []proto.Node{{Name: "echo", Addr: echoNode(t, "")}}, nil, hubOpts{proxies: true})
	addr := webProxyAddr(t, h)
	rsp, err := stdhttp.Get("http://" + addr + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer rsp.Body.Close()
	var st server.Status
	if err := json.NewDecoder(rsp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.Name != "hub" || len(st.Peers) != 2 {
		t.Fatalf("status through the proxy: %+v", st)
	}

	// A proxy with the wrong password gets nothing.
	bad := start(t, func(ctx context.Context, ready func(*client.Client)) error {
		return client.Run(ctx, client.Config{Name: "bad", WebProxies: []client.WebProxy{{Listen: "127.0.0.1:0", URL: "ws://" + h.server.WSAddr() + "/", Password: "wrong"}}}, "", io.Discard, ready)
	})
	c := &stdhttp.Client{Timeout: 5 * time.Second}
	if rsp, err := c.Get("http://" + webProxyAddr(t, &hub{client: bad}) + "/api/status"); err == nil {
		rsp.Body.Close()
		t.Fatalf("wrong password got %s", rsp.Status)
	}
}

func webProxyAddr(t *testing.T, h *hub) string {
	t.Helper()
	addr := h.client.WebProxyAddrs()
	if len(addr) != 1 {
		t.Fatalf("web proxies %v", addr)
	}
	return addr[0]
}

func TestWrongPassword(t *testing.T) {
	srv := start(t, func(ctx context.Context, ready func(*server.Server)) error {
		return server.Run(ctx, server.Config{Addr: "127.0.0.1:0", WebAddr: "127.0.0.1:0", Password: password}, io.Discard, ready)
	})
	p := start(t, func(ctx context.Context, ready func(*pod.Pod)) error {
		return pod.Run(ctx, pod.Config{Name: "lab", Servers: []uplink.Config{{Addr: srv.Addr(), Password: "wrong"}}}, "", io.Discard, ready)
	})
	waitFor(t, "the registration to fail", func() bool {
		st := p.Uplinks()[0].Status()
		return st.State == uplink.StateFailed && strings.Contains(st.Error, "authentication failed")
	})
	if n := len(srv.Status().Peers); n != 0 {
		t.Fatalf("%d peers registered", n)
	}
}

// Peers whose transport or body password does not match the server's do
// not register.
func TestMismatch(t *testing.T) {
	srv := start(t, func(ctx context.Context, ready func(*server.Server)) error {
		return server.Run(ctx, server.Config{Addr: "127.0.0.1:0", WebAddr: "127.0.0.1:0", ArpcWSAddr: "127.0.0.1:0", Password: password, BodyPassword: "aes-key"}, io.Discard, ready)
	})
	wsURL := srv.ArpcWSURL()
	cases := []struct {
		name string
		cfg  uplink.Config
		want string
	}{
		{"no body password", uplink.Config{Addr: srv.Addr(), Password: password}, ""},
		{"wrong body password", uplink.Config{Addr: wsURL, Password: password, BodyPassword: "other"}, ""},
		{"wrong websocket path", uplink.Config{Addr: strings.TrimSuffix(wsURL, "/fibhub") + "/nope", Password: password, BodyPassword: "aes-key"}, "404"},
		{"wss is not supported", uplink.Config{Addr: "wss://127.0.0.1:1/", Password: password}, "only ws://"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := start(t, func(ctx context.Context, ready func(*pod.Pod)) error {
				return pod.Run(ctx, pod.Config{Name: "lab", Servers: []uplink.Config{tc.cfg}}, "", io.Discard, ready)
			})
			waitFor(t, "the uplink to fail", func() bool {
				st := p.Uplinks()[0].Status()
				return st.State == uplink.StateFailed && strings.Contains(st.Error, tc.want)
			})
			time.Sleep(100 * time.Millisecond)
			if st := p.Uplinks()[0].Status(); st.State == uplink.StateRegistered {
				t.Fatal("registered")
			}
		})
	}
	if n := len(srv.Status().Peers); n != 0 {
		t.Fatalf("%d peers registered", n)
	}
	// The right settings over WebSocket do register.
	p := start(t, func(ctx context.Context, ready func(*pod.Pod)) error {
		return pod.Run(ctx, pod.Config{Name: "lab", Servers: []uplink.Config{{Addr: wsURL, Password: password, BodyPassword: "aes-key"}}}, "", io.Discard, ready)
	})
	waitFor(t, "the uplink to register", func() bool { return p.Uplinks()[0].Status().State == uplink.StateRegistered })
}

func TestParseForward(t *testing.T) {
	f, err := parseForward("127.0.0.1:2222=lab/ssh@hub")
	if err != nil || f != (client.Forward{Listen: "127.0.0.1:2222", Pod: "lab", Node: "ssh", Server: "hub"}) {
		t.Fatalf("%+v, %v", f, err)
	}
	for _, bad := range []string{"", "x", "1:2=lab", "1:2=/ssh", "=lab/ssh"} {
		if _, err := parseForward(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// A web page opens to its user and password, and to the cookie a post of
// them to /login gets; the client's page lists its web proxies with the
// server page's login.
func TestWebLogin(t *testing.T) {
	const auth = "admin:pa55"
	srv := start(t, func(ctx context.Context, ready func(*server.Server)) error {
		return server.Run(ctx, server.Config{Name: "hub", Addr: "127.0.0.1:0", WebAddr: "127.0.0.1:0", WSAddr: "127.0.0.1:0", WebAuth: auth, Password: password}, io.Discard, ready)
	})
	base := "http://" + srv.WebAddr()
	noRedirect := &stdhttp.Client{Timeout: 5 * time.Second, CheckRedirect: func(*stdhttp.Request, []*stdhttp.Request) error { return stdhttp.ErrUseLastResponse }}
	status := func(cookies ...*stdhttp.Cookie) int {
		req, _ := stdhttp.NewRequest("GET", base+"/api/status", nil)
		for _, c := range cookies {
			req.AddCookie(c)
		}
		rsp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		rsp.Body.Close()
		return rsp.StatusCode
	}
	login := func(user, pass string) []*stdhttp.Cookie {
		rsp, err := noRedirect.PostForm(base+"/login", url.Values{"user": {user}, "password": {pass}})
		if err != nil {
			t.Fatal(err)
		}
		rsp.Body.Close()
		if rsp.StatusCode != stdhttp.StatusSeeOther || rsp.Header.Get("Location") != "/" {
			t.Fatalf("login: %s to %q", rsp.Status, rsp.Header.Get("Location"))
		}
		return rsp.Cookies()
	}
	if got := status(); got != stdhttp.StatusUnauthorized {
		t.Fatalf("no login: %d", got)
	}
	if rsp, err := noRedirect.Get(base + "/login"); err != nil || rsp.StatusCode != stdhttp.StatusSeeOther {
		t.Fatalf("GET /login: %v %v", rsp, err)
	} else {
		rsp.Body.Close()
	}
	if c := login("admin", "wrong"); len(c) != 0 {
		t.Fatalf("a wrong password got cookies %v", c)
	}
	cookies := login("admin", "pa55")
	if len(cookies) != 1 || !cookies[0].HttpOnly {
		t.Fatalf("cookies %v", cookies)
	}
	if got := status(cookies...); got != stdhttp.StatusOK {
		t.Fatalf("with the cookie: %d", got)
	}
	forged := *cookies[0]
	forged.Value = strings.Replace(forged.Value, ".", ".0", 1)
	if got := status(&forged); got != stdhttp.StatusUnauthorized {
		t.Fatalf("with a forged cookie: %d", got)
	}
	if st := srv.Status(); st.Web.Auth != auth || st.Web.Addr == "" {
		t.Fatalf("server web %+v", st.Web)
	}

	c := start(t, func(ctx context.Context, ready func(*client.Client)) error {
		return client.Run(ctx, client.Config{Name: "me", WebProxies: []client.WebProxy{{Name: "hub", Listen: "127.0.0.1:0", URL: "ws://" + srv.WSAddr() + "/", Password: password, WebAuth: auth}}}, "", io.Discard, ready)
	})
	wps := c.Status().WebProxies
	if len(wps) != 1 || wps[0].Name != "hub" || wps[0].WebAuth != auth || wps[0].Addr == "" {
		t.Fatalf("client web proxies %+v", wps)
	}
	if err := c.SetWebAuth(wps[0].Listen, "root:x"); err != nil || c.Status().WebProxies[0].WebAuth != "root:x" {
		t.Fatalf("set web auth: %v, %+v", err, c.Status().WebProxies)
	}
	if err := c.SetWebAuth("127.0.0.1:1", "root:x"); err == nil {
		t.Fatal("set the web auth of nothing")
	}
	// The proxy's page takes a login of its own.
	base = "http://" + wps[0].Addr
	if got := status(login("admin", "pa55")...); got != stdhttp.StatusOK {
		t.Fatalf("through the proxy with the cookie: %d", got)
	}
}

// TestBalance has a pod and a client on two servers: every link reports its
// latency, and a forward that names no server spreads its tunnels over
// both.
func TestBalance(t *testing.T) {
	interval := uplink.PingInterval
	uplink.PingInterval = 50 * time.Millisecond
	t.Cleanup(func() { uplink.PingInterval = interval })

	var servers []uplink.Config
	var hubs []*server.Server
	for _, name := range []string{"hub-1", "hub-2"} {
		s := start(t, func(ctx context.Context, ready func(*server.Server)) error {
			return server.Run(ctx, server.Config{Name: name, Addr: "127.0.0.1:0", WebAddr: "127.0.0.1:0", Password: password}, io.Discard, ready)
		})
		hubs = append(hubs, s)
		servers = append(servers, uplink.Config{Name: name, Addr: s.Addr(), Password: password})
	}
	nodes := []proto.Node{{Name: "echo", Addr: echoNode(t, "")}}
	p := start(t, func(ctx context.Context, ready func(*pod.Pod)) error {
		return pod.Run(ctx, pod.Config{Name: "lab", Servers: servers, Nodes: nodes}, "", io.Discard, ready)
	})
	c := start(t, func(ctx context.Context, ready func(*client.Client)) error {
		return client.Run(ctx, client.Config{Name: "me", Servers: servers, Forwards: []client.Forward{{Listen: "127.0.0.1:0", Pod: "lab", Node: "echo"}}}, "", io.Discard, ready)
	})
	waitFor(t, "the client to measure both paths to the pod", func() bool {
		n := 0
		for _, s := range c.Status().Servers {
			for _, pv := range s.Pods {
				if s.RTT.Known() && pv.RTT.Known() {
					n++
				}
			}
		}
		return n == 2
	})
	for _, s := range hubs {
		waitFor(t, "the server to hear its peers' latency", func() bool {
			st := s.Status()
			for _, pv := range st.Peers {
				if !pv.RTT.Known() {
					return false
				}
			}
			return len(st.Peers) == 2
		})
	}

	addr := c.Forwards()[0].Addr
	var conns []net.Conn
	for range 6 {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.WriteString(conn, "ping"); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 4)
		if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
			t.Fatalf("%q, %v", buf, err)
		}
		conns = append(conns, conn)
	}
	per := map[string]int{}
	for _, tv := range c.Tunnels() {
		per[tv.Server]++
	}
	if per["hub-1"] == 0 || per["hub-2"] == 0 || per["hub-1"]+per["hub-2"] != len(conns) {
		t.Fatalf("tunnels per server: %v", per)
	}

	if !p.NodeRTT()["echo"].Known() {
		t.Fatal("the pod did not measure its connects to the node")
	}
	waitFor(t, "the client to hear of the pod's connects to the node", func() bool {
		for _, s := range c.Status().Servers {
			for _, pv := range s.Pods {
				if !pv.NodeRTT["echo"].Known() {
					return false
				}
			}
		}
		return true
	})
	for _, tv := range p.Tunnels() {
		if tv.ConnectRTT <= 0 {
			t.Fatalf("tunnel %d has no connect time", tv.ID)
		}
	}
}

// TestPool checks that pods and clients connect to a server with a pool of
// connections, that tunnels spread over the pool's connections, each on one
// of them, and that a pool connection that closes takes only its own
// tunnels with it.
func TestPool(t *testing.T) {
	h := startHub(t, []proto.Node{{Name: "echo", Addr: echoNode(t, "")}}, []client.Forward{{Listen: "127.0.0.1:0", Pod: "lab", Node: "echo"}}, hubOpts{})
	waitFor(t, "the pools to join", func() bool {
		if h.pod.Uplinks()[0].Status().Pool != uplink.DefaultPoolSize || h.client.Uplinks()[0].Status().Pool != uplink.DefaultPoolSize {
			return false
		}
		for _, p := range h.server.Status().Peers {
			if p.Conns != uplink.DefaultPoolSize {
				return false
			}
		}
		return true
	})
	addr := h.forwardAddr(t, "127.0.0.1:0")
	echo := func(c net.Conn, msg string) error {
		c.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.WriteString(c, msg); err != nil {
			return err
		}
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(c, buf); err != nil {
			return err
		}
		if string(buf) != msg {
			return fmt.Errorf("echoed %q", buf)
		}
		return nil
	}
	const n = 2 * uplink.DefaultPoolSize
	conns := make([]net.Conn, n)
	for i := range conns {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err := echo(c, fmt.Sprint("ping ", i)); err != nil {
			t.Fatal(err)
		}
		conns[i] = c
	}
	routes := h.server.Routes()
	clientLinks, podLinks := map[any]int{}, map[any]int{}
	for _, r := range routes {
		clientLinks[r.ClientLink]++
		podLinks[r.PodLink]++
	}
	if len(routes) != n || len(clientLinks) != uplink.DefaultPoolSize || len(podLinks) != uplink.DefaultPoolSize {
		t.Fatalf("%d tunnels over %d client and %d pod connections", len(routes), len(clientLinks), len(podLinks))
	}

	// Close a connection of the client's pool other than the one it
	// registered on, at the server.
	var me *server.Peer
	for _, p := range h.server.Peers() {
		if p.Info().Name == "me" {
			me = p
		}
	}
	var killed *server.Route
	for _, r := range routes {
		if r.ClientLink != me.Client {
			killed = r
			break
		}
	}
	gone := map[uint64]bool{}
	for _, r := range routes {
		if r.ClientLink == killed.ClientLink {
			gone[r.ID] = true
		}
	}
	byLocal := map[string]uint64{}
	for _, tv := range h.client.Tunnels() {
		byLocal[tv.Remote] = tv.ID
	}
	killed.ClientLink.Conn().Close()
	for i, c := range conns {
		id, ok := byLocal[c.LocalAddr().String()]
		if !ok {
			t.Fatalf("no tunnel for %s", c.LocalAddr())
		}
		err := echo(c, "again")
		if gone[id] != (err != nil) {
			t.Fatalf("tunnel %d (%d), on the closed connection: %v, echo: %v", id, i, gone[id], err)
		}
	}
	waitFor(t, "the pool to join again", func() bool {
		return h.client.Uplinks()[0].Status().Pool == uplink.DefaultPoolSize
	})
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := echo(c, "after"); err != nil {
		t.Fatal(err)
	}
}
