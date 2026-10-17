//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package server

import (
	"log/slog"
	stdhttp "net/http"
	"sync"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib/websocket"
	"github.com/lesismal/fibhub/internal/wscrypt"
)

// wsHandler serves the encrypted WebSocket: each connection, once its first
// message decrypts, gets a TCP connection of its own to the web page, and
// the two pass on what they receive, the WebSocket's decrypted, the TCP
// connection's encrypted.
func (s *Server) wsHandler() fib.Handler {
	secret, err := wscrypt.NewSecret(s.cfg.WSPassword)
	if err != nil {
		panic(err)
	}
	var mu sync.Mutex
	proxies := map[*websocket.Connection]*wsProxy{}
	get := func(ws *websocket.Connection) *wsProxy {
		mu.Lock()
		defer mu.Unlock()
		return proxies[ws]
	}
	return websocket.NewHandler(websocket.HandlerFuncs{
		Open: func(ws *websocket.Connection, r *stdhttp.Request) {
			p := &wsProxy{s: s, ws: ws, secret: secret, hello: wscrypt.Hello(), remote: r.RemoteAddr}
			mu.Lock()
			proxies[ws] = p
			mu.Unlock()
			_ = ws.WriteBinary(p.hello)
		},
		Message: func(ws *websocket.Connection, opcode websocket.Opcode, data []byte) {
			p := get(ws)
			if p == nil {
				return
			}
			if opcode != websocket.Binary {
				_ = ws.Close(websocket.CloseUnsupportedData, "binary messages only")
				return
			}
			p.message(data)
		},
		Close: func(ws *websocket.Connection, _ uint16, _ string, _ error) {
			mu.Lock()
			p := proxies[ws]
			delete(proxies, ws)
			mu.Unlock()
			if p != nil {
				p.close()
			}
		},
	})
}

// wsProxy is one WebSocket connection and its TCP connection to the web
// page.
type wsProxy struct {
	s      *Server
	ws     *websocket.Connection
	secret wscrypt.Secret
	hello  []byte
	remote string

	mu      sync.Mutex
	sess    *wscrypt.Session
	up      *fib.Connection
	dialing bool
	pending [][]byte
	closed  bool
}

func (p *wsProxy) message(data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	if p.sess == nil {
		sess, err := wscrypt.NewSession(p.secret, p.hello, data, true)
		if err != nil {
			_ = p.ws.Close(websocket.ClosePolicyViolation, "bad hello")
			return
		}
		p.sess = sess
		return
	}
	plain, err := p.sess.Open(data)
	if err != nil {
		slog.Warn("fibhub: websocket message failed to decrypt", "remote", p.remote)
		_ = p.ws.Close(websocket.ClosePolicyViolation, "decryption failed")
		return
	}
	if p.up != nil {
		_ = p.up.Send(plain)
		return
	}
	p.pending = append(p.pending, plain)
	if p.dialing {
		return
	}
	p.dialing = true
	err = p.s.engine.DialWithHandler("tcp", p.s.webAddr, 5*time.Second, &wsUpstream{p: p}, func(_ *fib.Connection, err error) {
		if err != nil {
			_ = p.ws.Close(websocket.CloseInternalError, "web page unreachable")
		}
	})
	if err != nil {
		_ = p.ws.Close(websocket.CloseInternalError, "web page unreachable")
	}
}

func (p *wsProxy) close() {
	p.mu.Lock()
	p.closed = true
	up := p.up
	p.mu.Unlock()
	if up != nil {
		up.CloseAfterSend()
	}
}

// wsUpstream is the handler of a wsProxy's TCP connection.
type wsUpstream struct{ p *wsProxy }

func (u *wsUpstream) OnOpen(c *fib.Connection) {
	p := u.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		_ = c.Close()
		return
	}
	p.up = c
	for _, b := range p.pending {
		_ = c.Send(b)
	}
	p.pending = nil
}

func (u *wsUpstream) OnData(_ *fib.Connection, data []byte) {
	p := u.p
	p.mu.Lock()
	sess := p.sess
	p.mu.Unlock()
	_ = p.ws.WriteBinary(sess.Seal(data))
}

func (u *wsUpstream) OnPriorityData(*fib.Connection, []byte) {}

func (u *wsUpstream) OnClose(*fib.Connection, error) {
	_ = u.p.ws.Close(websocket.CloseNormal, "web page closed the connection")
}
