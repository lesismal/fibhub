//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package server

import (
	_ "embed"
	"errors"
	stdhttp "net/http"
	"strconv"
	"time"

	fib "github.com/lesismal/fib"
	fibhttp "github.com/lesismal/fib/http"
	"github.com/lesismal/fibhub/internal/common"
	"github.com/lesismal/fibhub/internal/proto"
)

//go:embed web/index.html
var indexHTML []byte

// PeerView is a peer as the web page shows it.
type PeerView struct {
	ID      uint32     `json:"id"`
	Addr    string     `json:"addr"`
	Since   time.Time  `json:"since"`
	Info    proto.Info `json:"info"`
	Tunnels int        `json:"tunnels"`
	// Conns is how many connections the peer has registered, and Places
	// the places they hold, lowest first.
	Conns  int   `json:"conns"`
	Places []int `json:"places"`
	proto.PodLatency
}

// RouteView is a tunnel as the web page shows it.
type RouteView struct {
	ID      uint64    `json:"id,string"`
	Client  string    `json:"client"`
	Pod     string    `json:"pod"`
	Node    string    `json:"node"`
	Addr    string    `json:"addr"`
	Created time.Time `json:"created"`
	Up      int64     `json:"up"`
	Down    int64     `json:"down"`
}

// Status is the server's state, as the web page shows it.
type Status struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Addr    string `json:"addr"`
	WSAddr  string `json:"wsAddr,omitempty"`
	ArpcWS  string `json:"arpcWs,omitempty"`
	// BodyEncrypted reports whether arpc bodies are encrypted.
	BodyEncrypted bool `json:"bodyEncrypted"`
	// Web is this page's address and login.
	Web     common.WebPage `json:"web"`
	Started time.Time      `json:"started"`
	Peers   []PeerView     `json:"peers"`
	Tunnels []RouteView    `json:"tunnels"`
}

// Status is the server's state.
func (s *Server) Status() Status {
	st := Status{Name: s.cfg.Name, Version: proto.Version, Addr: s.Addr(), WSAddr: s.WSAddr(), ArpcWS: s.ArpcWSURL(), BodyEncrypted: s.cfg.BodyPassword != "", Web: common.WebPage{Addr: s.webListen, Auth: s.cfg.WebAuth}, Started: s.started, Peers: []PeerView{}, Tunnels: []RouteView{}}
	counts := map[*Peer]int{}
	for _, r := range s.Routes() {
		counts[r.Client]++
		counts[r.Pod]++
		s.mu.Lock()
		addr := r.Addr
		s.mu.Unlock()
		st.Tunnels = append(st.Tunnels, RouteView{
			ID: r.ID, Client: r.Client.Info().Name, Pod: r.Pod.Info().Name, Node: r.Node, Addr: addr,
			Created: r.Created, Up: r.up.Load(), Down: r.down.Load(),
		})
	}
	for _, p := range s.Peers() {
		s.mu.Lock()
		places := p.placesLocked()
		s.mu.Unlock()
		st.Peers = append(st.Peers, PeerView{ID: p.ID, Addr: p.Addr, Since: p.Since, Info: p.Info(), Tunnels: counts[p], Conns: len(places), Places: places, PodLatency: p.Latency()})
	}
	return st
}

func (s *Server) webHandler() fib.Handler {
	r := fibhttp.NewRouter()
	r.Use(common.BasicAuth(s.cfg.WebAuth))
	common.Static(r)
	r.Get("/", common.Page(indexHTML))
	r.Get("/api/status", func(c *fibhttp.Context) { common.JSON(c, stdhttp.StatusOK, s.Status()) })
	r.Post("/api/peers/{id}/refresh", func(c *fibhttp.Context) {
		pod, ok := s.podParam(c)
		if !ok {
			return
		}
		common.Async(c, func() {
			info, err := s.refreshPod(pod)
			if err != nil {
				common.Error(c, stdhttp.StatusBadGateway, err)
				return
			}
			common.JSON(c, stdhttp.StatusOK, info)
		})
	})
	r.Post("/api/peers/{id}/nodes", func(c *fibhttp.Context) {
		pod, ok := s.podParam(c)
		if !ok {
			return
		}
		var nodes []proto.Node
		if err := common.Bind(c, &nodes); err != nil {
			common.Error(c, stdhttp.StatusBadRequest, err)
			return
		}
		common.Async(c, func() {
			info, err := s.setPodNodes(pod, nodes)
			if err != nil {
				common.Error(c, stdhttp.StatusBadGateway, err)
				return
			}
			common.JSON(c, stdhttp.StatusOK, info)
		})
	})
	r.Post("/api/peers/{id}/kick", func(c *fibhttp.Context) {
		peer := s.peerParam(c)
		if peer == nil {
			return
		}
		s.kick(peer, "kicked from the web page")
		common.OK(c)
	})
	r.Post("/api/tunnels/{id}/close", func(c *fibhttp.Context) {
		id, err := strconv.ParseUint(c.Param("id"), 10, 64)
		if err != nil {
			common.Error(c, stdhttp.StatusBadRequest, err)
			return
		}
		if !s.CloseRoute(id, "closed from the server's web page") {
			common.Error(c, stdhttp.StatusNotFound, errors.New("no such tunnel"))
			return
		}
		common.OK(c)
	})
	return fibhttp.NewHandler(r)
}

func (s *Server) peerParam(c *fibhttp.Context) *Peer {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		common.Error(c, stdhttp.StatusBadRequest, err)
		return nil
	}
	peer := s.peerByID(uint32(id))
	if peer == nil {
		common.Error(c, stdhttp.StatusNotFound, errors.New("no such peer"))
	}
	return peer
}

func (s *Server) podParam(c *fibhttp.Context) (*Peer, bool) {
	peer := s.peerParam(c)
	if peer == nil {
		return nil, false
	}
	if peer.Info().Type != proto.TypePod {
		common.Error(c, stdhttp.StatusBadRequest, errors.New("not a pod"))
		return nil, false
	}
	return peer, true
}
