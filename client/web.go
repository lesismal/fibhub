//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package client

import (
	_ "embed"
	"errors"
	stdhttp "net/http"
	"strconv"

	fib "github.com/lesismal/fib"
	fibhttp "github.com/lesismal/fib/http"
	"github.com/lesismal/fibhub/internal/common"
	"github.com/lesismal/fibhub/internal/proto"
	"github.com/lesismal/fibhub/internal/uplink"
)

//go:embed web/index.html
var indexHTML []byte

// ServerView is a server and its pods, as the web page shows them.
type ServerView struct {
	uplink.Status
	Pods []PodView `json:"pods"`
}

// PodView is a pod of a server, as the web page shows it.
type PodView struct {
	proto.PodView
	// Share is the part of the tunnels to the pod that should go through
	// the server, from 0 to 1, by Weights.
	Share float64 `json:"share"`
}

// Status is the client's state, as the web page shows it.
type Status struct {
	Name     string            `json:"name"`
	Version  string            `json:"version"`
	Saved    bool              `json:"saved"`
	Meta     map[string]string `json:"meta"`
	Servers  []ServerView      `json:"servers"`
	Forwards []ForwardView     `json:"forwards"`
	// WebProxies serve servers' web pages locally.
	WebProxies []WebProxyView `json:"webProxies"`
	Tunnels    []TunnelView   `json:"tunnels"`
}

// Status is the client's state.
func (c *Client) Status() Status {
	info := c.Info()
	st := Status{Name: info.Name, Version: proto.Version, Saved: c.configPath != "", Meta: info.Meta, Servers: []ServerView{}, Forwards: c.Forwards(), WebProxies: c.WebProxies(), Tunnels: c.Tunnels()}
	// The paths to each pod, to weigh them against each other.
	paths := map[string][]Path{}
	for _, u := range c.Uplinks() {
		v := ServerView{Status: u.Status(), Pods: []PodView{}}
		if list := c.Pods(u); list != nil && v.State == uplink.StateRegistered {
			for _, p := range list.Pods {
				v.Pods = append(v.Pods, PodView{PodView: p})
				paths[p.Info.Name] = append(paths[p.Info.Name], Path{Server: v.RTT, Pod: p.RTT})
			}
		}
		st.Servers = append(st.Servers, v)
	}
	weights := map[string][]float64{}
	for name, ps := range paths {
		weights[name] = Weights(ps)
	}
	// The servers in the same order again, so each takes its pods' weights.
	for i := range st.Servers {
		for j := range st.Servers[i].Pods {
			p := &st.Servers[i].Pods[j]
			w := weights[p.Info.Name]
			p.Share, weights[p.Info.Name] = w[0], w[1:]
		}
	}
	return st
}

func (c *Client) webHandler() fib.Handler {
	r := fibhttp.NewRouter()
	r.Use(common.BasicAuth(c.cfg.WebAuth))
	common.Static(r)
	r.Get("/", common.Page(indexHTML))
	r.Get("/api/status", func(ctx *fibhttp.Context) { common.JSON(ctx, stdhttp.StatusOK, c.Status()) })
	r.Post("/api/servers", func(ctx *fibhttp.Context) {
		var sc uplink.Config
		if bind(ctx, &sc) {
			reply(ctx, c.AddServer(sc))
		}
	})
	r.Post("/api/servers/delete", func(ctx *fibhttp.Context) {
		var req struct {
			Name string `json:"name"`
		}
		if bind(ctx, &req) {
			reply(ctx, c.RemoveServer(req.Name))
		}
	})
	r.Post("/api/pods/refresh", func(ctx *fibhttp.Context) {
		var req struct {
			Server string `json:"server"`
		}
		if !bind(ctx, &req) {
			return
		}
		u := c.uplink(req.Server)
		if u == nil {
			common.Error(ctx, stdhttp.StatusNotFound, errors.New("no such server"))
			return
		}
		common.Async(ctx, func() {
			list, err := c.RefreshPods(u)
			if err != nil {
				common.Error(ctx, stdhttp.StatusBadGateway, err)
				return
			}
			common.JSON(ctx, stdhttp.StatusOK, list)
		})
	})
	r.Post("/api/pods/nodes", func(ctx *fibhttp.Context) {
		var req struct {
			Server string       `json:"server"`
			Pod    string       `json:"pod"`
			Nodes  []proto.Node `json:"nodes"`
		}
		if !bind(ctx, &req) {
			return
		}
		u := c.uplink(req.Server)
		if u == nil {
			common.Error(ctx, stdhttp.StatusNotFound, errors.New("no such server"))
			return
		}
		common.Async(ctx, func() {
			info, err := c.SetPodNodes(u, req.Pod, req.Nodes)
			if err != nil {
				common.Error(ctx, stdhttp.StatusBadGateway, err)
				return
			}
			common.JSON(ctx, stdhttp.StatusOK, info)
		})
	})
	r.Post("/api/meta", func(ctx *fibhttp.Context) {
		var meta map[string]string
		if bind(ctx, &meta) {
			c.SetMeta(meta)
			common.OK(ctx)
		}
	})
	r.Post("/api/forwards", func(ctx *fibhttp.Context) {
		var f Forward
		if bind(ctx, &f) {
			reply(ctx, c.AddForward(f))
		}
	})
	r.Post("/api/forwards/delete", func(ctx *fibhttp.Context) {
		var req struct {
			Listen string `json:"listen"`
		}
		if bind(ctx, &req) {
			reply(ctx, c.RemoveForward(req.Listen))
		}
	})
	r.Post("/api/webauth", func(ctx *fibhttp.Context) {
		var req struct {
			Listen  string `json:"listen"`
			WebAuth string `json:"webAuth"`
		}
		if bind(ctx, &req) {
			reply(ctx, c.SetWebAuth(req.Listen, req.WebAuth))
		}
	})
	r.Post("/api/tunnels/close", func(ctx *fibhttp.Context) {
		var req struct {
			Server string `json:"server"`
			ID     string `json:"id"`
		}
		if !bind(ctx, &req) {
			return
		}
		id, err := strconv.ParseUint(req.ID, 10, 64)
		if err != nil {
			common.Error(ctx, stdhttp.StatusBadRequest, err)
			return
		}
		u := c.uplink(req.Server)
		if u == nil {
			common.Error(ctx, stdhttp.StatusNotFound, errors.New("no such server"))
			return
		}
		e := u.Tunnels.Get(id)
		if e == nil {
			common.Error(ctx, stdhttp.StatusNotFound, errors.New("no such tunnel"))
			return
		}
		e.Close("closed from the client's web page")
		common.OK(ctx)
	})
	return fibhttp.NewHandler(r)
}

func bind(ctx *fibhttp.Context, v any) bool {
	if err := common.Bind(ctx, v); err != nil {
		common.Error(ctx, stdhttp.StatusBadRequest, err)
		return false
	}
	return true
}

func reply(ctx *fibhttp.Context, err error) {
	if err != nil {
		common.Error(ctx, stdhttp.StatusBadRequest, err)
		return
	}
	common.OK(ctx)
}
