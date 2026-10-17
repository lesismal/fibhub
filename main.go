//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command fibhub tunnels TCP connections to devices behind pods, through a
// hub, on fib's engine. -mode picks one of three modes:
//
//	server  the hub: pods and clients register with it over arpc, and it
//	        routes each tunnel a client opens to its pod
//	pod     runs next to the nodes (devices) and connects to them for the
//	        tunnels the server routes to it
//	client  listens on local ports and tunnels each connection to a node
//	        through the server and the node's pod
//
// For example, to reach the SSH server of a device 10.0.0.5 that only a pod
// host can reach:
//
//	fibhub -mode=server -password=s3cret
//	fibhub -mode=pod    -name=lab -password=s3cret -server=hub.example:7000 -node=ssh=10.0.0.5:22
//	fibhub -mode=client -name=me  -password=s3cret -server=hub.example:7000 -forward=127.0.0.1:2222=lab/ssh
//	ssh -p 2222 127.0.0.1
//
// Where only HTTP gets through, the server also serves arpc over WebSocket
// (-arpc-ws), and pods and clients name it as ws://host:port/fibhub. With
// -body-password on all three, every arpc message body is AES-encrypted.
//
// Settings can live in a JSON file given with -config, whose "mode" stands
// in for -mode; -print-config prints a template of one. Flags given on the
// command line override the file. See README.md for deployment.
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/lesismal/fibhub/client"
	"github.com/lesismal/fibhub/internal/common"
	"github.com/lesismal/fibhub/internal/proto"
	"github.com/lesismal/fibhub/internal/uplink"
	"github.com/lesismal/fibhub/internal/wsarpc"
	"github.com/lesismal/fibhub/pod"
	"github.com/lesismal/fibhub/server"
)

//go:embed configs/*.json
var templates embed.FS

// placeholder is the password the templates carry, which fibhub refuses to
// run with.
const placeholder = "CHANGE-ME"

var modes = []string{"server", "pod", "client"}

// options are the command line.
type options struct {
	mode, config            string
	help, version, template bool

	name, web, webAuth      string
	password, bodyPassword  string
	addr, ws, wsPassword    string
	arpcWS, arpcWSPath      string
	servers, meta           common.StringList
	nodes, forwards, wproxy common.StringList

	flags   *flag.FlagSet
	visited map[string]bool
}

// flagModes are the modes a flag is for; a flag not in it is for all.
var flagModes = map[string][]string{
	"addr":         {"server"},
	"ws":           {"server"},
	"arpc-ws":      {"server"},
	"arpc-ws-path": {"server"},
	"server":       {"pod", "client"},
	"meta":         {"pod", "client"},
	"node":         {"pod"},
	"forward":      {"client"},
	"webproxy":     {"client"},
	"ws-password":  {"server", "client"},
}

// usageGroups list the flags in the order -help shows them.
var usageGroups = []struct {
	title string
	names []string
}{
	{"General", []string{"mode", "config", "print-config", "help", "version"}},
	{"All modes", []string{"name", "web", "web-auth", "password", "body-password"}},
	{"Server", []string{"addr", "arpc-ws", "arpc-ws-path", "ws", "ws-password"}},
	{"Pod and client", []string{"server", "meta"}},
	{"Pod", []string{"node"}},
	{"Client", []string{"forward", "webproxy", "ws-password"}},
}

func newOptions(out io.Writer) *options {
	o := &options{}
	f := flag.NewFlagSet("fibhub", flag.ContinueOnError)
	f.SetOutput(out)
	f.StringVar(&o.mode, "mode", "", "server, pod or client (default: the config file's \"mode\")")
	f.StringVar(&o.config, "config", "", "JSON settings file; a pod or client also saves its web page's changes to it")
	f.BoolVar(&o.template, "print-config", false, "print a config file template for -mode and exit")
	f.BoolVar(&o.help, "help", false, "show this help")
	f.BoolVar(&o.version, "version", false, "print the version and exit")

	f.StringVar(&o.name, "name", "", "name told to peers (default: \"fibhub\" for a server, the host name otherwise)")
	f.StringVar(&o.web, "web", "", "address of the web page, empty to disable it for a pod or client (default: 127.0.0.1:7080 server, 127.0.0.1:7180 pod, 127.0.0.1:7280 client)")
	f.StringVar(&o.webAuth, "web-auth", "", "user:password protecting the web page")
	f.StringVar(&o.password, "password", os.Getenv("FIBHUB_PASSWORD"), "server: password pods and clients register with; pod, client: password of the -server servers (default $FIBHUB_PASSWORD)")
	f.StringVar(&o.bodyPassword, "body-password", os.Getenv("FIBHUB_BODY_PASSWORD"), "AES-encrypt every arpc message body with a key from this password, the same on every side (default $FIBHUB_BODY_PASSWORD, empty: off)")

	f.StringVar(&o.addr, "addr", ":7000", "arpc address pods and clients connect to")
	f.StringVar(&o.arpcWS, "arpc-ws", "", "address pods and clients reach arpc on over WebSocket (empty: none)")
	f.StringVar(&o.arpcWSPath, "arpc-ws-path", "/fibhub", "path of the arpc WebSocket")
	f.StringVar(&o.ws, "ws", "", "address of the encrypted WebSocket carrying the web page (empty: none)")
	f.StringVar(&o.wsPassword, "ws-password", "", "password encrypting the web page's WebSocket: the server's -ws, the client's -webproxy (default: -password)")

	f.Var(&o.servers, "server", "server to connect to, [name=]host:port or ws://host:port/path (repeatable)")
	f.Var(&o.meta, "meta", "key=value told to the servers (repeatable)")
	f.Var(&o.nodes, "node", "node the pod can reach, name=host:port[,description] (repeatable)")
	f.Var(&o.forwards, "forward", "local address tunneled to a node, listen=pod/node[@server] (repeatable)")
	f.Var(&o.wproxy, "webproxy", "serve a server's web page locally through its encrypted WebSocket, listen=ws://host:port/ (repeatable)")
	f.Usage = func() { o.usage(out) }
	o.flags = f
	return o
}

func (o *options) usage(out io.Writer) {
	fmt.Fprint(out, `fibhub tunnels TCP connections to devices behind pods, through a hub.

Usage:
  fibhub -mode=server|pod|client [flags]
  fibhub -config=FILE [flags]
  fibhub -mode=MODE -print-config > FILE
  fibhub -version
`)
	for _, g := range usageGroups {
		fmt.Fprintf(out, "\n%s:\n", g.title)
		for _, name := range g.names {
			f := o.flags.Lookup(name)
			fmt.Fprintf(out, "  -%s\n    \t%s", f.Name, f.Usage)
			if f.DefValue != "" && f.DefValue != "false" && !strings.Contains(f.Usage, "(default") {
				fmt.Fprintf(out, " (default %q)", f.DefValue)
			}
			fmt.Fprintln(out)
		}
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "fibhub:", err)
		os.Exit(2)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	o := newOptions(out)
	if err := o.flags.Parse(args); err != nil {
		return err
	}
	if o.flags.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q; every option is a flag, such as -mode=server", o.flags.Args())
	}
	o.visited = map[string]bool{}
	o.flags.Visit(func(f *flag.Flag) { o.visited[f.Name] = true })
	switch {
	case o.help:
		o.usage(out)
		return nil
	case o.version:
		fmt.Fprintln(out, "fibhub", proto.Version)
		return nil
	}

	mode := o.mode
	if mode == "" && o.config != "" {
		var file struct {
			Mode string `json:"mode"`
		}
		if err := common.LoadJSON(o.config, &file); err != nil {
			return err
		}
		mode = file.Mode
	}
	if mode == "" {
		o.usage(out)
		return errors.New("no mode: give -mode=server, -mode=pod or -mode=client, or a -config file with a \"mode\"")
	}
	if !slices.Contains(modes, mode) {
		return fmt.Errorf("unknown mode %q: use server, pod or client", mode)
	}
	if o.template {
		b, err := templates.ReadFile("configs/" + mode + ".json")
		if err != nil {
			return err
		}
		_, err = out.Write(b)
		return err
	}
	for name := range o.visited {
		if allowed, ok := flagModes[name]; ok && !slices.Contains(allowed, mode) {
			return fmt.Errorf("-%s is not a flag of mode %s, only of %s", name, mode, strings.Join(allowed, " and "))
		}
	}
	switch mode {
	case "server":
		return o.runServer(ctx, out)
	case "pod":
		return o.runPod(ctx, out)
	default:
		return o.runClient(ctx, out)
	}
}

// set has a flag replace what the config file says: when it was given, or
// when the file left the setting empty.
func (o *options) set(name string, dst *string, value string) {
	if o.visited[name] || *dst == "" {
		*dst = value
	}
}

// checkPasswords refuses the templates' placeholder.
func checkPasswords(passwords ...string) error {
	for _, p := range passwords {
		if strings.Contains(p, placeholder) {
			return fmt.Errorf("a password is still the template's %s; set real ones first", placeholder)
		}
	}
	return nil
}

func (o *options) runServer(ctx context.Context, out io.Writer) error {
	var cfg server.Config
	if err := common.LoadJSON(o.config, &cfg); err != nil {
		return err
	}
	if cfg.Mode != "" && cfg.Mode != "server" {
		return fmt.Errorf("%s is a %s config, not a server's", o.config, cfg.Mode)
	}
	o.set("name", &cfg.Name, o.name)
	o.set("addr", &cfg.Addr, o.addr)
	o.set("web", &cfg.WebAddr, o.web)
	o.set("web-auth", &cfg.WebAuth, o.webAuth)
	o.set("ws", &cfg.WSAddr, o.ws)
	o.set("ws-password", &cfg.WSPassword, o.wsPassword)
	o.set("arpc-ws", &cfg.ArpcWSAddr, o.arpcWS)
	o.set("arpc-ws-path", &cfg.ArpcWSPath, o.arpcWSPath)
	o.set("password", &cfg.Password, o.password)
	o.set("body-password", &cfg.BodyPassword, o.bodyPassword)
	if err := checkPasswords(cfg.Password, cfg.BodyPassword, cfg.WSPassword, cfg.WebAuth); err != nil {
		return err
	}
	return server.Run(ctx, cfg, out, nil)
}

// peer puts the flags pods and clients share over what the config file
// set: the name, web address and meta replace the file's, and -server
// servers are added unless the file has one of the same name.
func (o *options) peer(defaultWeb string, name, web, webAuth *string, servers *[]uplink.Config, meta *map[string]string) error {
	if o.visited["name"] || *name == "" {
		*name = o.name
		if *name == "" {
			*name, _ = os.Hostname()
		}
	}
	// A config file may leave the web page out on purpose, with an empty
	// webAddr; without a file the default applies.
	switch {
	case o.visited["web"]:
		*web = o.web
	case o.config == "" || !fileExists(o.config):
		*web = defaultWeb
	}
	o.set("web-auth", webAuth, o.webAuth)
	for _, s := range o.servers {
		sc := uplink.Config{Name: s, Addr: s, Password: o.password, BodyPassword: o.bodyPassword}
		if n, addr, ok := strings.Cut(s, "="); ok && !wsarpc.IsURL(s) {
			sc.Name, sc.Addr = n, addr
		}
		if sc.Password == "" {
			return fmt.Errorf("-server %s needs a -password", s)
		}
		if !slices.ContainsFunc(*servers, func(have uplink.Config) bool { return have.Name == sc.Name }) {
			*servers = append(*servers, sc)
		}
	}
	if len(o.meta) > 0 {
		m := map[string]string{}
		for _, kv := range o.meta {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return fmt.Errorf("-meta %q is not key=value", kv)
			}
			m[k] = v
		}
		*meta = m
	}
	passwords := []string{*webAuth}
	for _, sc := range *servers {
		passwords = append(passwords, sc.Password, sc.BodyPassword)
	}
	return checkPasswords(passwords...)
}

func (o *options) runPod(ctx context.Context, out io.Writer) error {
	var cfg pod.Config
	if err := common.LoadJSON(o.config, &cfg); err != nil {
		return err
	}
	if cfg.Mode != "" && cfg.Mode != "pod" {
		return fmt.Errorf("%s is a %s config, not a pod's", o.config, cfg.Mode)
	}
	if err := o.peer("127.0.0.1:7180", &cfg.Name, &cfg.WebAddr, &cfg.WebAuth, &cfg.Servers, &cfg.Meta); err != nil {
		return err
	}
	for _, n := range o.nodes {
		name, rest, ok := strings.Cut(n, "=")
		if !ok {
			return fmt.Errorf("-node %q is not name=host:port", n)
		}
		node := proto.Node{Name: name}
		node.Addr, node.Desc, _ = strings.Cut(rest, ",")
		if i := slices.IndexFunc(cfg.Nodes, func(have proto.Node) bool { return have.Name == name }); i >= 0 {
			cfg.Nodes[i] = node
		} else {
			cfg.Nodes = append(cfg.Nodes, node)
		}
	}
	return pod.Run(ctx, cfg, o.config, out, nil)
}

func (o *options) runClient(ctx context.Context, out io.Writer) error {
	var cfg client.Config
	if err := common.LoadJSON(o.config, &cfg); err != nil {
		return err
	}
	if cfg.Mode != "" && cfg.Mode != "client" {
		return fmt.Errorf("%s is a %s config, not a client's", o.config, cfg.Mode)
	}
	if err := o.peer("127.0.0.1:7280", &cfg.Name, &cfg.WebAddr, &cfg.WebAuth, &cfg.Servers, &cfg.Meta); err != nil {
		return err
	}
	for _, s := range o.forwards {
		f, err := parseForward(s)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(cfg.Forwards, func(have client.Forward) bool { return have.Listen == f.Listen }) {
			cfg.Forwards = append(cfg.Forwards, f)
		}
	}
	password := o.wsPassword
	if password == "" {
		password = o.password
	}
	for _, s := range o.wproxy {
		listen, url, ok := strings.Cut(s, "=")
		if !ok || listen == "" || url == "" {
			return fmt.Errorf("-webproxy %q is not listen=ws://host:port/", s)
		}
		if password == "" {
			return fmt.Errorf("-webproxy %s needs a -ws-password or -password", s)
		}
		cfg.WebProxies = append(cfg.WebProxies, client.WebProxy{Listen: listen, URL: url, Password: password})
	}
	for _, wp := range cfg.WebProxies {
		if err := checkPasswords(wp.Password); err != nil {
			return err
		}
	}
	return client.Run(ctx, cfg, o.config, out, nil)
}

// parseForward parses listen=pod/node[@server].
func parseForward(s string) (client.Forward, error) {
	listen, target, ok := strings.Cut(s, "=")
	target, server, _ := strings.Cut(target, "@")
	podName, node, ok2 := strings.Cut(target, "/")
	if !ok || !ok2 || listen == "" || podName == "" || node == "" {
		return client.Forward{}, fmt.Errorf("-forward %q is not listen=pod/node[@server]", s)
	}
	return client.Forward{Listen: listen, Pod: podName, Node: node, Server: server}, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
