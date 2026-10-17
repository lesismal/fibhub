# fibhub

[![CI](https://github.com/lesismal/fibhub/actions/workflows/ci.yml/badge.svg)](https://github.com/lesismal/fibhub/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/lesismal/fibhub.svg)](https://pkg.go.dev/github.com/lesismal/fibhub)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[English](README.md)

fibhub 用来访问只有“身边的机器”才能连到的设备（**node**）上的 TCP 服务：经由一个大家都能连到的中心节点转发。
它基于 [fib](https://github.com/lesismal/fib) 实现：控制和隧道流量走 arpc，Web 页面用 fib 的 HTTP 路由，加密的页面代理用 fib 的 WebSocket。

```text
 本地应用 ──TCP──▶ fibhub client ══arpc══▶ fibhub server ══arpc══▶ fibhub pod ──TCP──▶ node
 (ssh、浏览器)      每个 forward 监听        中心：路由隧道、          部署在设备旁边      设备
                   一个本地端口             提供管理页面                                  10.0.0.5:22
```

| 模式 | 部署位置 | 作用 |
|------|----------|------|
| `server` | pod 和 client 都能连到的主机 | 认证并登记 pod、client；把 client 发起的隧道路由给指定的 pod 并转发隧道数据；提供管理页面 |
| `pod` | 与设备同网络的主机 | 向一个或多个 server 注册自己以及可到达的 node；为路由过来的每条隧道连接 node（只连配置过的 node） |
| `client` | 用户自己的机器 | 向 server 注册，获取 pod 和 node 列表；每个 *forward* 监听一个本地端口，每个连进来的连接变成一条隧道 |

pod 和 client 都是**主动向外**连接 server，两者都不需要开放入站端口。

## 安装

需要 Go 1.27 及以上。安装最新版本到 `$(go env GOPATH)/bin`：

```sh
go install github.com/lesismal/fibhub@latest
```

或者克隆本仓库后编译：

```sh
git clone https://github.com/lesismal/fibhub && cd fibhub
go build -o fibhub .
# 交叉编译，例如给 Linux 上的 pod 主机：
GOOS=linux GOARCH=amd64 go build -o fibhub-linux-amd64 .
GOOS=linux GOARCH=arm64 go build -o fibhub-linux-arm64 .
```

支持 Linux、macOS、BSD 和 Windows。产物是单个二进制文件，Web 页面已内嵌。

## 快速体验

在一台机器上，把本机的 SSH 服务当作“设备”来访问：

```sh
fibhub -mode=server -password=s3cret
fibhub -mode=pod    -name=lab -password=s3cret -server=127.0.0.1:7000 -node=ssh=127.0.0.1:22
fibhub -mode=client -name=me  -password=s3cret -server=127.0.0.1:7000 -forward=127.0.0.1:2222=lab/ssh
ssh -p 2222 127.0.0.1
```

Web 页面：server <http://127.0.0.1:7080>，pod <http://127.0.0.1:7180>，client <http://127.0.0.1:7280>。

## 命令行参数

所有参数都是 flag：`fibhub -mode=server|pod|client [flags]`、`fibhub -help`、`fibhub -version`。

| 参数 | 适用模式 | 含义 |
|------|----------|------|
| `-mode` | 全部 | `server`、`pod` 或 `client`；也可以由配置文件里的 `"mode"` 指定 |
| `-config` | 全部 | JSON 配置文件（见[配置文件](#配置文件)） |
| `-print-config` | 全部 | 打印 `-mode` 对应的配置模板后退出 |
| `-name` | 全部 | 告诉对端的名字；server 默认 `fibhub`，其他默认主机名 |
| `-web` | 全部 | Web 页面地址，默认 `127.0.0.1:7080` / `:7180` / `:7280`；pod、client 设为空则关闭 |
| `-web-auth` | 全部 | Web 页面的 HTTP Basic 认证，`user:password` |
| `-password` | 全部 | server：pod 和 client 注册用的密码；pod/client：`-server` 指定的 server 的密码。默认取 `$FIBHUB_PASSWORD` |
| `-body-password` | 全部 | 用 AES 加密每条 arpc 消息的 body；server、pod、client 必须相同。默认取 `$FIBHUB_BODY_PASSWORD` |
| `-addr` | server | arpc 的 TCP 地址，默认 `:7000` |
| `-arpc-ws` | server | 以 WebSocket 承载 arpc 的地址，用于只放行 HTTP 的网络 |
| `-arpc-ws-path` | server | 上述 WebSocket 的路径，默认 `/fibhub` |
| `-ws` | server | 承载 Web 页面的加密 WebSocket 地址 |
| `-ws-password` | server、client | 该 WebSocket 的密码，默认同 `-password` |
| `-server` | pod、client | 要连接的 server，`[name=]host:port` 或 `ws://host:port/fibhub`；可重复 |
| `-meta` | pod、client | 告诉 server 的 `key=value`；可重复 |
| `-node` | pod | pod 可连接的 node，`name=host:port[,描述]`；可重复 |
| `-forward` | client | `listen=pod/node[@server]`，如 `127.0.0.1:2222=lab/ssh`；可重复 |
| `-webproxy` | client | `listen=ws://host:port/`：通过 server 的 `-ws` 在本地提供其 Web 页面 |

使用不属于当前模式的参数会报错；不是 flag 的位置参数也会报错。

## 配置文件

先打印模板，修改后运行：

```sh
fibhub -mode=server -print-config > server.json
fibhub -mode=pod    -print-config > pod.json
fibhub -mode=client -print-config > client.json
vi server.json                     # 替换所有 CHANGE-ME
fibhub -config=server.json         # 文件里的 "mode" 决定运行模式
```

同样的模板也在 [`configs/`](configs) 目录。只要还有密码是 `CHANGE-ME`，fibhub 就拒绝启动。字段（JSON 键）：

- **server**：`name`、`addr`、`arpcWsAddr`、`arpcWsPath`、`webAddr`、`webAuth`、`wsAddr`、`wsPassword`、
  `password`、`bodyPassword`
- **pod**：`name`、`webAddr`、`webAuth`、`servers[]`（`name`、`addr`、`password`、`bodyPassword`）、
  `nodes[]`（`name`、`addr`、`desc`）、`meta`
- **client**：`name`、`webAddr`、`webAuth`、`servers[]`、`forwards[]`（`listen`、`pod`、`node`、可选的
  `server`、可选的 `webAuth`）、`webProxies[]`（可选的 `name`、`listen`、`url`、`password`、可选的
  `webAuth`）、`meta`。forward 或 web proxy 的 `webAuth` 是它所到达的 web 页面（pod 的、server 的）的
  `user:password`，client 页面会显示它，并用它打开该页面。

优先级：命令行上给出的 flag 覆盖配置文件；`-server`、`-node`、`-forward` 是追加，文件中已有同名条目则不重复添加。
pod 和 client 在 Web 页面上修改设置后会**写回配置文件**（启动时也会写一次，使命令行参数得以保留），
文件权限为 `0600`，因为其中包含密码。server 只读取配置文件。

## 部署

### 端口

| 模式 | 默认 | 开放给 | 用途 |
|------|------|--------|------|
| server | `:7000` | pod、client | arpc over TCP |
| server | `-arpc-ws`，如 `:7001` | pod、client | arpc over WebSocket（可选） |
| server | `127.0.0.1:7080` | 仅运维 | Web 页面 |
| server | `-ws`，如 `:7081` | 运维 | 加密的 Web 页面代理（可选） |
| pod | `127.0.0.1:7180` | 仅本机 | Web 页面 |
| client | `127.0.0.1:7280` | 仅本机 | Web 页面 |
| client | 每个 forward 的 `listen` | 本地应用 | 隧道入口 |

### server

```sh
sudo install -m 755 fibhub /usr/local/bin/fibhub
sudo useradd --system --no-create-home --shell /usr/sbin/nologin fibhub
sudo install -d -o fibhub -g fibhub -m 700 /etc/fibhub
fibhub -mode=server -print-config | sudo -u fibhub tee /etc/fibhub/server.json >/dev/null
sudo chmod 600 /etc/fibhub/server.json   # 然后设置各项密码
```

`/etc/systemd/system/fibhub-server.service`：

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

防火墙放行 `:7000`（设置了 `arpcWsAddr` 时再放行 `:7001`）。Web 页面保持监听 `127.0.0.1`，
通过 `ssh -L 7080:127.0.0.1:7080 hub` 或下文的加密代理访问。

### pod

把二进制拷贝到能访问设备的主机上。pod 会把 Web 页面上的修改保存到配置文件，因此运行用户必须能写该文件：

```sh
sudo install -m 755 fibhub /usr/local/bin/fibhub
sudo useradd --system --no-create-home --shell /usr/sbin/nologin fibhub
sudo install -d -o fibhub -g fibhub -m 700 /etc/fibhub
fibhub -mode=pod -print-config | sudo -u fibhub tee /etc/fibhub/pod.json >/dev/null
sudo -u fibhub vi /etc/fibhub/pod.json
```

`/etc/systemd/system/fibhub-pod.service` 与 server 的 unit 相同，只是
`ExecStart=/usr/local/bin/fibhub -config=/etc/fibhub/pod.json`。目录属于 `fibhub` 用户，pod 才能写回配置文件。

pod 只会连接配置中列出的 node。需要访问多个网络时部署多个不同名字的 pod；在 `servers[]` 中配置多个 server 可做冗余。

### client

通常手动运行或作为用户服务运行：

```sh
fibhub -mode=client -print-config > ~/.config/fibhub-client.json   # 编辑
fibhub -config=$HOME/.config/fibhub-client.json
```

然后让本地应用连接 forward 的 `listen` 地址（`ssh -p 2222 127.0.0.1`）。client 页面会列出各 server 的 pod 和
node，点 node 旁的 “Forward…” 即可填好一个新的 forward。

### 只放行 HTTP 的网络 / 反向代理之后

server 加 `-arpc-ws=:7001`，pod 和 client 使用 `ws://hub.example.com:7001/fibhub`。经由 nginx：

```nginx
location /fibhub {
    proxy_pass http://127.0.0.1:7001;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_read_timeout 1d;
}
```

fibhub 目前只支持 `ws://`，不支持 `wss://`。请设置 `bodyPassword`，使隧道数据在这种情况下仍然加密。

### 远程访问 server 的 Web 页面

server 启动时加 `-ws=:7081`（可另设 `-ws-password`）。在自己的机器上：

```sh
fibhub -mode=client -web= -ws-password=... -webproxy=127.0.0.1:7090=ws://hub.example.com:7081/
```

然后打开 <http://127.0.0.1:7090>。每个连接都用该密码派生的密钥做 AES-256-GCM 加密；密码不对则什么也拿不到。

## Web 页面

- **server**：pod（及其 node）、client、隧道及上下行字节数。可以查询 pod、编辑其 node（保存到 pod 上）、
  踢掉某个 peer、关闭隧道。
- **pod**：已连接的 server 及状态、可到达的 node（编辑并保存，所有 server 都会收到更新）、隧道。
- **client**：已连接的 server、各 server 的 pod 与 node（可经 server 编辑某个 pod 的 node）、forward
  （运行中增删）、web proxy、隧道，以及告诉 server 的 meta。带 `webAuth` 的 forward 或 web proxy 会显示
  该页面的用户名和密码（默认打码，点 **Show** 显示），以及一个 **Open** 按钮，点击即免登录打开该页面。

server 和 pod 页面也会显示本节点 web 页面的地址、用户名和密码，同样带 **Open** 按钮。**Open** 把用户名和
密码 POST 到该页面的 `/login`，后者返回一个会话 cookie（30 天，用该页面的 `webAuth` 签名），之后无需再
输入；basic auth 照常可用。

## 安全说明

- **注册认证**使用密码对 peer 名字和一次性 nonce 做 HMAC-SHA256；密码本身从不在网络上传输，截获的注册请求也
  无法重放。
- **未在所有端设置 `-body-password` 时，隧道数据和控制消息是明文传输的。** 设置后，每条 arpc 消息的 body 都用
  AES-256-GCM 加密（密钥由密码经 PBKDF2-SHA256 派生），未加密或解密失败的消息会导致连接被关闭。目前还没有
  TLS / `wss://`。
- **pod** 只连接自己配置中列出的 node，无论 client 请求什么。
- **Web 页面**默认监听 `127.0.0.1`；对外开放前请设置 `-web-auth`。pod 和 client 的页面可以修改其 server 和 node。
- **配置文件**包含密码，写入时权限为 `0600`。

## 常见问题

| 现象 | 可能原因 |
|------|----------|
| server 页面看不到 pod；pod 页面显示 `register: fibhub: authentication failed` | pod 的 `password` 与 server 不一致 |
| pod/client 不断重连；server 日志有 `bodycrypt: ... unencrypted` 或 `failed to decrypt` | `bodyPassword` 不一致，或只有一端设置了 |
| `ws://` 地址报 `bad websocket handshake: 404` | 路径不对，须与 `-arpc-ws-path` 一致 |
| 连到 forward 的本地连接立刻断开；client 日志有 `no server has pod "x" with node "y"` | pod 未注册或没有该 node；在 client 页面查看 pod 列表 |
| pod 日志有 `tunnel open failed ... connection refused` | node 地址错误或设备未启动 |
| `a password is still the template's CHANGE-ME` | 先修改配置文件中的密码 |

日志输出到 stderr。

## 开发

```text
fibhub/
├── main.go            命令入口：flag、配置文件、模式选择
├── server/            中心节点：注册、路由、隧道转发、Web 页面、加密页面代理
├── pod/               pod：向 server 注册、node 列表、连接 node
├── client/            client：forward、本地监听、Web 页面代理
├── internal/
│   ├── proto/         各模式共用的 arpc 方法、消息与版本号
│   ├── tunnel/        隧道的一端，每条隧道独立流控
│   ├── uplink/        pod 或 client 到某个 server 的连接，自动重连
│   ├── wsarpc/        基于 WebSocket 的 arpc
│   ├── bodycrypt/     arpc 消息 body 的 AES-256-GCM 加密
│   ├── wscrypt/       承载 server Web 页面的加密 WebSocket
│   └── common/        Web 页面共用的部分：JSON 辅助、页面认证、配置文件读写、静态文件
└── configs/           配置文件模板（与 -print-config 输出相同）
```

```sh
go vet ./...
go test -race ./...
```

端到端测试（`e2e_test.go`）会在一个进程内用所有传输方式运行 server、pod 和 client；`cli_test.go` 覆盖命令行与配置文件。

fibhub 只依赖 fib，而 fib 除标准库外没有任何依赖。想用本地的 fib 代码调试时，在两者旁边建一个 workspace：

```sh
go work init . ../fib
```

## 许可证

MIT，见 [LICENSE](LICENSE)。
