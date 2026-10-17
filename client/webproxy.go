//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package client

import (
	"log/slog"
	stdhttp "net/http"
	"sync"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib/websocket"
	"github.com/lesismal/fibhub/internal/wscrypt"
)

// WebProxy serves a server's web page on a local address, through the
// server's encrypted WebSocket: each connection to Listen gets a WebSocket
// of its own to URL, which carries what the two exchange, encrypted with
// Password.
type WebProxy struct {
	// Name names the server, for the web page.
	Name     string `json:"name,omitempty"`
	Listen   string `json:"listen"`
	URL      string `json:"url"`
	Password string `json:"password"`
	// WebAuth is the server page's user:password, which the client's web
	// page shows and opens the page with.
	WebAuth string `json:"webAuth,omitempty"`
}

// startWebProxy listens for wp, and returns the address it listens on.
func (c *Client) startWebProxy(wp WebProxy) (string, error) {
	secret, err := wscrypt.NewSecret(wp.Password)
	if err != nil {
		return "", err
	}
	h := &webProxyHandler{wp: wp, secret: secret, dialer: websocket.NewDialer(c.engine, websocket.DefaultDialerConfig())}
	i, err := c.engine.AddBinding(fib.Binding{Addr: wp.Listen, Handler: h})
	if err != nil {
		return "", err
	}
	addrs, err := c.engine.BindingAddrs(i)
	if err != nil || len(addrs) == 0 {
		return wp.Listen, nil
	}
	return addrs[0].String(), nil
}

// webProxyHandler is the handler of a WebProxy's local connections.
type webProxyHandler struct {
	wp     WebProxy
	secret wscrypt.Secret
	dialer *websocket.Dialer
}

// webProxyConn is a local connection and its WebSocket.
type webProxyConn struct {
	local  *fib.Connection
	secret wscrypt.Secret

	mu      sync.Mutex
	ws      *websocket.Connection
	sess    *wscrypt.Session
	pending [][]byte
	closed  bool
}

func (h *webProxyHandler) OnOpen(local *fib.Connection) {
	pc := &webProxyConn{local: local, secret: h.secret}
	local.SetAttachment(pc)
	h.dialer.Dial(h.wp.URL, nil, websocket.HandlerFuncs{
		Open:    func(ws *websocket.Connection, _ *stdhttp.Request) { pc.opened(ws) },
		Message: func(_ *websocket.Connection, _ websocket.Opcode, data []byte) { pc.message(data) },
		Close:   func(*websocket.Connection, uint16, string, error) { local.CloseAfterSend() },
	}, func(_ *websocket.Connection, _ *stdhttp.Response, err error) {
		if err != nil {
			slog.Warn("fibhub: web proxy dial failed", "url", h.wp.URL, "err", err)
			_ = local.Close()
		}
	})
}

func (pc *webProxyConn) opened(ws *websocket.Connection) {
	pc.mu.Lock()
	pc.ws = ws
	closed := pc.closed
	pc.mu.Unlock()
	if closed {
		_ = ws.Close(websocket.CloseNormal, "")
	}
}

// message is the server's hello, and then what the page sends, encrypted.
func (pc *webProxyConn) message(data []byte) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.sess == nil {
		hello := wscrypt.Hello()
		sess, err := wscrypt.NewSession(pc.secret, data, hello, false)
		if err != nil {
			_ = pc.ws.Close(websocket.ClosePolicyViolation, "bad hello")
			return
		}
		pc.sess = sess
		_ = pc.ws.WriteBinary(hello)
		for _, b := range pc.pending {
			_ = pc.ws.WriteBinary(sess.Seal(b))
		}
		pc.pending = nil
		return
	}
	plain, err := pc.sess.Open(data)
	if err != nil {
		slog.Warn("fibhub: web proxy message failed to decrypt; is the password right?")
		_ = pc.ws.Close(websocket.ClosePolicyViolation, "decryption failed")
		return
	}
	_ = pc.local.Send(plain)
}

func (h *webProxyHandler) OnData(local *fib.Connection, data []byte) {
	pc, ok := local.Attachment().(*webProxyConn)
	if !ok {
		return
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.sess == nil {
		pc.pending = append(pc.pending, append([]byte(nil), data...))
		return
	}
	_ = pc.ws.WriteBinary(pc.sess.Seal(data))
}

func (h *webProxyHandler) OnPriorityData(*fib.Connection, []byte) {}

func (h *webProxyHandler) OnClose(local *fib.Connection, _ error) {
	pc, ok := local.Attachment().(*webProxyConn)
	if !ok {
		return
	}
	pc.mu.Lock()
	pc.closed = true
	ws := pc.ws
	pc.mu.Unlock()
	if ws != nil {
		_ = ws.Close(websocket.CloseNormal, "")
	}
}
