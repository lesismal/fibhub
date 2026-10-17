//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package pod

import (
	_ "embed"
	"errors"
	stdhttp "net/http"
	"strconv"

	fib "github.com/lesismal/fib"
	fibhttp "github.com/lesismal/fib/http"
	"github.com/lesismal/fibhub/internal/common"
	"github.com/lesismal/fibhub/internal/latency"
	"github.com/lesismal/fibhub/internal/proto"
	"github.com/lesismal/fibhub/internal/uplink"
)

//go:embed web/index.html
var indexHTML []byte

// Status is the pod's state, as the web page shows it.
type Status struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Saved   bool   `json:"saved"`
	// Web is this page's address and login.
	Web   common.WebPage `json:"web"`
	Nodes []proto.Node   `json:"nodes"`
	// NodeRTT is, by node name, how long the connects to the nodes took.
	NodeRTT map[string]latency.Stats `json:"nodeRtt"`
	Servers []uplink.Status          `json:"servers"`
	Tunnels []TunnelView             `json:"tunnels"`
}

// Status is the pod's state.
func (p *Pod) Status() Status {
	cfg := p.Config()
	st := Status{Name: cfg.Name, Version: proto.Version, Saved: p.configPath != "", Web: common.WebPage{Addr: p.webAddr, Auth: cfg.WebAuth}, Nodes: cfg.Nodes, NodeRTT: p.NodeRTT(), Servers: []uplink.Status{}, Tunnels: p.Tunnels()}
	for _, u := range p.Uplinks() {
		st.Servers = append(st.Servers, u.Status())
	}
	return st
}

func (p *Pod) webHandler() fib.Handler {
	r := fibhttp.NewRouter()
	r.Use(common.BasicAuth(p.cfg.WebAuth))
	common.Static(r)
	r.Get("/", common.Page(indexHTML))
	r.Get("/api/status", func(c *fibhttp.Context) { common.JSON(c, stdhttp.StatusOK, p.Status()) })
	r.Post("/api/nodes", func(c *fibhttp.Context) {
		var nodes []proto.Node
		if err := common.Bind(c, &nodes); err != nil {
			common.Error(c, stdhttp.StatusBadRequest, err)
			return
		}
		if err := p.SetNodes(nodes, nil); err != nil {
			common.Error(c, stdhttp.StatusBadRequest, err)
			return
		}
		common.OK(c)
	})
	r.Post("/api/servers", func(c *fibhttp.Context) {
		var sc uplink.Config
		if err := common.Bind(c, &sc); err != nil {
			common.Error(c, stdhttp.StatusBadRequest, err)
			return
		}
		if err := p.AddServer(sc); err != nil {
			common.Error(c, stdhttp.StatusBadRequest, err)
			return
		}
		common.OK(c)
	})
	r.Post("/api/servers/delete", func(c *fibhttp.Context) {
		var req struct {
			Name string `json:"name"`
		}
		if err := common.Bind(c, &req); err != nil {
			common.Error(c, stdhttp.StatusBadRequest, err)
			return
		}
		if err := p.RemoveServer(req.Name); err != nil {
			common.Error(c, stdhttp.StatusNotFound, err)
			return
		}
		common.OK(c)
	})
	r.Post("/api/tunnels/close", func(c *fibhttp.Context) {
		var req struct {
			Server string `json:"server"`
			ID     string `json:"id"`
		}
		if err := common.Bind(c, &req); err != nil {
			common.Error(c, stdhttp.StatusBadRequest, err)
			return
		}
		id, err := strconv.ParseUint(req.ID, 10, 64)
		if err != nil {
			common.Error(c, stdhttp.StatusBadRequest, err)
			return
		}
		u := p.uplink(req.Server)
		if u == nil {
			common.Error(c, stdhttp.StatusNotFound, errors.New("no such server"))
			return
		}
		e := u.Tunnels.Get(id)
		if e == nil {
			common.Error(c, stdhttp.StatusNotFound, errors.New("no such tunnel"))
			return
		}
		e.Close("closed from the pod's web page")
		common.OK(c)
	})
	return fibhttp.NewHandler(r)
}
