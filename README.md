# fibhub

[![CI](https://github.com/lesismal/fibhub/actions/workflows/ci.yml/badge.svg)](https://github.com/lesismal/fibhub/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/lesismal/fibhub.svg)](https://pkg.go.dev/github.com/lesismal/fibhub)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[中文](README.zh-CN.md)

fibhub reaches TCP services on devices (**nodes**) that only a machine next to them can reach, through a
hub anyone can reach. It is built on [fib](https://github.com/lesismal/fib): arpc for control and tunnel
traffic, fib's HTTP router for the web pages, its WebSocket for the encrypted page proxy.

```text
 local app ──TCP──▶ fibhub client ══arpc══▶ fibhub server ══arpc══▶ fibhub pod ──TCP──▶ node
 (ssh, browser)     listens on a local      the hub: routes          runs next to       device
                    port per forward        tunnels, web page        the nodes          10.0.0.5:22
```

| Mode | Runs on | Does |
|------|---------|------|
| `server` | a host pods and clients can reach | authenticates and registers pods and clients, routes each tunnel a client opens to the pod it names, relays the tunnel's frames, serves an admin web page |
| `pod` | a host on the devices' network | registers with one or more servers and the nodes it can reach, dials a node for every tunnel routed to it (only nodes it is configured with) |
| `client` | the user's machine | registers with servers, learns their pods and nodes, listens on a local port per *forward* and turns every connection there into a tunnel |

Pods and clients connect **out** to the server, so neither needs an open inbound port.

## Install

Go 1.27 or later. Install the latest release into `$(go env GOPATH)/bin`:

```sh
go install github.com/lesismal/fibhub@latest
```

or build from a clone of this repository:

```sh
git clone https://github.com/lesismal/fibhub && cd fibhub
go build -o fibhub .
# cross-compile, e.g. for a Linux pod host:
GOOS=linux GOARCH=amd64 go build -o fibhub-linux-amd64 .
GOOS=linux GOARCH=arm64 go build -o fibhub-linux-arm64 .
```

It runs on Linux, macOS, the BSDs and Windows. It is one static binary; the web pages are embedded.

## Quick start

On one machine, reaching a local SSH server as if it were a device:

```sh
fibhub -mode=server -password=s3cret
fibhub -mode=pod    -name=lab -password=s3cret -server=127.0.0.1:7000 -node=ssh=127.0.0.1:22
fibhub -mode=client -name=me  -password=s3cret -server=127.0.0.1:7000 -forward=127.0.0.1:2222=lab/ssh
ssh -p 2222 127.0.0.1
```

Web pages: server <http://127.0.0.1:7080>, pod <http://127.0.0.1:7180>, client <http://127.0.0.1:7280>.

## Command line

Everything is a flag: `fibhub -mode=server|pod|client [flags]`, `fibhub -help`, `fibhub -version`.

| Flag | Modes | Meaning |
|------|-------|---------|
| `-mode` | all | `server`, `pod` or `client`; may come from the config file's `"mode"` instead |
| `-config` | all | JSON settings file (see [Config files](#config-files)) |
| `-print-config` | all | print a template config for `-mode` and exit |
| `-name` | all | name told to peers; default `fibhub` for a server, the host name otherwise |
| `-web` | all | web page address; default `127.0.0.1:7080` / `:7180` / `:7280`; empty disables a pod's or client's |
| `-web-auth` | all | `user:password` HTTP basic auth on the web page |
| `-password` | all | server: the password pods and clients register with; pod/client: the password of the `-server` servers. Default `$FIBHUB_PASSWORD` |
| `-body-password` | all | AES-encrypt every arpc message body; must be the same on server, pods and clients. Default `$FIBHUB_BODY_PASSWORD` |
| `-addr` | server | arpc TCP address, default `:7000` |
| `-arpc-ws` | server | address serving arpc over WebSocket, for networks that only pass HTTP |
| `-arpc-ws-path` | server | its path, default `/fibhub` |
| `-ws` | server | address of the encrypted WebSocket carrying the web page |
| `-ws-password` | server, client | password of that WebSocket; default `-password` |
| `-server` | pod, client | server to connect to, `[name=]host:port` or `ws://host:port/fibhub`; repeatable |
| `-meta` | pod, client | `key=value` told to the servers; repeatable |
| `-node` | pod | node the pod may reach, `name=host:port[,description]`; repeatable |
| `-forward` | client | `listen=pod/node[@server]`, e.g. `127.0.0.1:2222=lab/ssh`; repeatable |
| `-webproxy` | client | `listen=ws://host:port/`: serve a server's web page locally through its `-ws` |

A flag that belongs to another mode is an error, as is any word that is not a flag.

## Config files

Print a template, edit it, run it:

```sh
fibhub -mode=server -print-config > server.json
fibhub -mode=pod    -print-config > pod.json
fibhub -mode=client -print-config > client.json
vi server.json                     # replace every CHANGE-ME
fibhub -config=server.json         # "mode" in the file picks the mode
```

The same templates are in [`configs/`](configs). fibhub refuses to start while any password is still
`CHANGE-ME`. Fields (JSON keys):

- **server**: `name`, `addr`, `arpcWsAddr`, `arpcWsPath`, `webAddr`, `webAuth`, `wsAddr`, `wsPassword`,
  `password`, `bodyPassword`
- **pod**: `name`, `webAddr`, `webAuth`, `servers[]` (`name`, `addr`, `password`, `bodyPassword`),
  `nodes[]` (`name`, `addr`, `desc`), `meta`
- **client**: `name`, `webAddr`, `webAuth`, `servers[]`, `forwards[]` (`listen`, `pod`, `node`, optional
  `server`, optional `webAuth`), `webProxies[]` (optional `name`, `listen`, `url`, `password`, optional
  `webAuth`), `meta`. A forward's or web proxy's `webAuth` is the `user:password` of the web page it
  reaches (a pod's, a server's), which the client's page shows and opens that page with.

Precedence: a flag given on the command line overrides the file; `-server`, `-node` and `-forward` add
entries unless the file has one of the same name. A pod or client **writes the file back** when its
settings change on its web page (and once at start, so flags given then are kept), with mode `0600`
since it holds passwords. A server only reads its file.

### Generating a deployment's configs

[`configs/gen-configs.sh`](configs/gen-configs.sh) writes matching config files for a whole deployment
at once (every server, pod and client), with random passwords that agree with each other:

1. List the instances in [`configs/config.sh`](configs/config.sh). The file explains each list and
   has examples:

   ```sh
   SERVERS=(            # "name ip": ip is where pods and clients reach that server
     "hub-1 127.0.0.1"  # replace 127.0.0.1 with each server's address
     "hub-2 127.0.0.1"
   )
   PODS=("lab" "office")
   CLIENTS=("alice-laptop" "bob-macmini")
   ```

2. Optionally change the other settings at the top of `gen-configs.sh`:

   | Setting | Default | Meaning |
   |---------|---------|---------|
   | `UPLINK` | `ws` | pods and clients reach servers over `ws` (`ws://ip:10001/fibhub`) or `tcp` (`ip:10000`) |
   | `NODES`, `NODE_ADDRS` | `ssh` 127.0.0.1:22, `web` 127.0.0.1:{web} | nodes every pod offers; `{web}` is the pod's own web port |
   | `FORWARD_NODES` | `ssh web` | nodes every client forwards, for every pod |
   | `WEB_USER` | `admin` | user of every web page's login |
   | `PASSWORD_LEN` | `24` | length of the random passwords |
   | `*_PORT`, `CLIENT_PORTS` | see below | ports |

3. Run it; the files go to `configs/gen/` (ignored by git), or to the directory given:

   ```sh
   ./configs/gen-configs.sh            # configs/gen/server-hub-1.json, pod-lab.json, client-alice-laptop.json, ...
   ./configs/gen-configs.sh /tmp/out
   ```

4. Copy each file to its host and start it, e.g. with [`restart.sh`](restart.sh), which stops the
   running process of that mode, starts `fibhub` (by default the binary next to the script) with the
   file, and logs to the file's name with `.log`:

   ```sh
   ./restart.sh configs/gen/server-hub-1.json
   ```

What each instance gets:

- **server**: `addr` `:10000`, `arpcWsAddr` `:10001`, `webAddr` `127.0.0.1:10002`, `wsAddr` `:10003`, and
  its own `password`, `bodyPassword`, `wsPassword` and `webAuth`.
- **pod**: `webAddr` `:10006`, its `webAuth`, every server, and the nodes from `NODES`.
- **client**: `webAddr` `127.0.0.1:10011`, its `webAuth`, every server; then, from `CLIENT_PORTS`
  (`10012`-`10015`), a web proxy per server (with the server page's `webAuth`), then a forward per pod
  and `FORWARD_NODES` entry (a pod web page's forward has its `webAuth`), so the client's web page can
  open them logged in.

Things to know:

- Every run makes **new passwords** for every file: deploy the whole set together.
- Instances of one mode share ports, so each runs on its own host; `restart.sh` also stops every
  running process of that mode first. Lists with several `127.0.0.1` entries are templates: replace the
  addresses, or change the ports of those sharing a host by hand.
- A client needs `servers + pods × FORWARD_NODES` ports; with more than `CLIENT_PORTS` holds, the rest
  are skipped with a warning. Add ports to `CLIENT_PORTS` for larger deployments.

## Deployment

### Ports

| Mode | Default | Expose to | Purpose |
|------|---------|-----------|---------|
| server | `:7000` | pods and clients | arpc over TCP |
| server | `-arpc-ws`, e.g. `:7001` | pods and clients | arpc over WebSocket (optional) |
| server | `127.0.0.1:7080` | operators only | web page |
| server | `-ws`, e.g. `:7081` | operators | encrypted web page proxy (optional) |
| pod | `127.0.0.1:7180` | local only | web page |
| client | `127.0.0.1:7280` | local only | web page |
| client | each forward's `listen` | local apps | tunnels |

### Server

```sh
sudo install -m 755 fibhub /usr/local/bin/fibhub
sudo useradd --system --no-create-home --shell /usr/sbin/nologin fibhub
sudo install -d -o fibhub -g fibhub -m 700 /etc/fibhub
fibhub -mode=server -print-config | sudo -u fibhub tee /etc/fibhub/server.json >/dev/null
sudo chmod 600 /etc/fibhub/server.json   # then set the passwords
```

`/etc/systemd/system/fibhub-server.service`:

```ini
[Unit]
Description=fibhub server
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/fibhub -config=/etc/fibhub/server.json
User=fibhub
Restart=always
RestartSec=3
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload && sudo systemctl enable --now fibhub-server
journalctl -u fibhub-server -f
```

Open `:7000` (and `:7001` if `arpcWsAddr` is set) in the firewall. Keep the web page on `127.0.0.1`, and
reach it with `ssh -L 7080:127.0.0.1:7080 hub` or the encrypted proxy below.

### Pod

Copy the binary to a host that reaches the devices. A pod saves its web page's changes to its config,
so the file must be writable by the user it runs as:

```sh
sudo install -m 755 fibhub /usr/local/bin/fibhub
sudo useradd --system --no-create-home --shell /usr/sbin/nologin fibhub
sudo install -d -o fibhub -g fibhub -m 700 /etc/fibhub
fibhub -mode=pod -print-config | sudo -u fibhub tee /etc/fibhub/pod.json >/dev/null
sudo -u fibhub vi /etc/fibhub/pod.json
```

`/etc/systemd/system/fibhub-pod.service` is the server's unit with
`ExecStart=/usr/local/bin/fibhub -config=/etc/fibhub/pod.json`. The directory belongs to `fibhub`, so the
pod can write the file back.

A pod connects only to the nodes listed in its config. Run several pods with different names to reach several
networks, and several servers in `servers[]` for redundancy.

### Client

Usually run by hand or as a user service:

```sh
fibhub -mode=client -print-config > ~/.config/fibhub-client.json   # edit
fibhub -config=$HOME/.config/fibhub-client.json
```

Then point the local app at a forward's `listen` address (`ssh -p 2222 127.0.0.1`). The client page lists
every server's pods and nodes; "Forward…" on a node fills in a new forward.

### Behind an HTTP-only network or a reverse proxy

Serve arpc over WebSocket with `-arpc-ws=:7001` and have peers use `ws://hub.example.com:7001/fibhub`.
Through nginx:

```nginx
location /fibhub {
    proxy_pass http://127.0.0.1:7001;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_read_timeout 1d;
}
```

fibhub dials `ws://` only, not `wss://`. Set `bodyPassword` so the tunnel data is encrypted anyway.

### Reaching the server's web page remotely

Start the server with `-ws=:7081` (and optionally `-ws-password`). On your machine:

```sh
fibhub -mode=client -web= -ws-password=... -webproxy=127.0.0.1:7090=ws://hub.example.com:7081/
```

and open <http://127.0.0.1:7090>. Each connection is AES-256-GCM encrypted with keys derived from that
password; a wrong password gets nothing.

## Web pages

- **server**: pods (with nodes), clients, tunnels with bytes up/down. Query a pod, edit its nodes (saved
  on the pod), kick a peer, close a tunnel.
- **pod**: its servers and their state, the nodes it can reach (edit and save; every server hears of the
  change), its tunnels.
- **client**: its servers, their pods and nodes (edit a pod's nodes through the server), its forwards
  (add/remove while running), its web proxies, its tunnels, and the meta it tells servers. Each server's
  row shows its web page, through the web proxy named after it (or else on its host). A web proxy or
  forward shows its page's user and password (hidden until **Show**), an **Open** button that opens the
  page already logged in, and **Set login** to change the `webAuth` it keeps in the config.

The server and pod pages also show their own address, user and password, with the same **Open**.
**Open** posts the user and password to the page's `/login`, which answers with a session cookie (30
days, signed with the page's `webAuth`), so the page asks for nothing; basic auth works as before.

## Security

- **Registration** is a HMAC-SHA256 of the password over the peer's name and a fresh nonce; the password
  never crosses the wire, and a captured registration cannot be replayed.
- **Tunnel data and control messages travel in the clear unless `-body-password` is set** on all sides.
  With it, every arpc message body is AES-256-GCM encrypted (key from PBKDF2-SHA256 of the password), and
  a message that is unencrypted or does not decrypt closes the connection. There is no TLS or `wss://`
  yet.
- **Pods** only dial nodes they list, whatever a client asks for.
- **Web pages** listen on `127.0.0.1` by default; set `-web-auth` before exposing one. A pod's and a
  client's pages can change their servers and nodes.
- **Config files** hold passwords; they are written `0600`.

## Troubleshooting

| Symptom | Likely cause |
|---------|--------------|
| server page shows no pod; pod page says `register: fibhub: authentication failed` | pod's `password` differs from the server's |
| pod/client keeps reconnecting; server logs `bodycrypt: ... unencrypted` or `failed to decrypt` | `bodyPassword` differs, or is set on only one side |
| `ws://` server: `bad websocket handshake: 404` | wrong path; it must match `-arpc-ws-path` |
| local connection to a forward closes at once; client logs `no server has pod "x" with node "y"` | the pod is not registered, or lacks that node; check the client page's pod list |
| `tunnel open failed ... connection refused` in the pod log | the node's address is wrong or the device is down |
| `a password is still the template's CHANGE-ME` | edit the config file first |

Logs go to stderr.

## Development

```text
fibhub/
├── main.go            the command: flags, config files, mode selection
├── server/            the hub: registration, routing, tunnel relay, web page, encrypted page proxy
├── pod/               the pod: registration with servers, node list, dialing nodes
├── client/            the client: forwards, local listeners, web page proxy
├── internal/
│   ├── proto/         the arpc methods and messages every mode shares, and the version
│   ├── tunnel/        one end of a tunnel, with per-tunnel flow control
│   ├── uplink/        a pod's or client's connection to one server, with reconnects
│   ├── wsarpc/        arpc over WebSocket
│   ├── bodycrypt/     AES-256-GCM for arpc message bodies
│   ├── wscrypt/       the encrypted WebSocket that carries the server's web page
│   └── common/        what the web pages share: JSON helpers, page auth, settings files, static files
└── configs/           config file templates (the same as -print-config)
```

```sh
go vet ./...
go test -race ./...
```

The end-to-end tests (`e2e_test.go`) run server, pod and client in one process over every transport;
`cli_test.go` covers the command line and config files.

fibhub depends only on fib, which has no dependencies outside the standard library. To try it against
a local fib checkout, add a workspace next to both:

```sh
go work init . ../fib
```

## License

MIT, see [LICENSE](LICENSE).
