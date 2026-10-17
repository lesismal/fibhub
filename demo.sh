#!/usr/bin/env bash
# demo.sh runs a whole fibhub deployment on this machine, to try it out or
# show it: several servers, pods and clients, each pod and client connected
# to every server, all on 127.0.0.1.
#
#   ./demo.sh [start] [-s servers] [-p pods] [-c clients] [--open]
#   ./demo.sh stop
#   ./demo.sh status
#
# start (the default) builds fibhub from this checkout, writes a config per
# instance with fresh random passwords, stops a demo that is running and
# starts every instance, then prints each web page with its login. Counts
# default to 2 servers, 2 pods and 2 clients (1-9 each). --open opens the
# first client's page in the browser; it lists every server's page (through
# its web proxy) and every pod's page (through a forward), and opens them
# logged in.
#
# Every pod offers its own web page as node "web" and 127.0.0.1:22 as node
# "ssh"; every client forwards both for every pod. Pods and clients reach
# odd-numbered servers over TCP and even-numbered ones over WebSocket, with
# encrypted bodies. Opening a pod's page through a client's forward shows
# up as tunnels on every page.
#
# Environment: DEMO_DIR (default .demo next to this script) holds the
# binary, configs, logs and pid files; DEMO_PORT (default 20000) is the
# first port: servers use DEMO_PORT+10*i.., pods DEMO_PORT+100+i, clients
# DEMO_PORT+200+50*i..; FIBHUB_BIN uses that binary instead of building.
#
# demo.sh 在本机运行一整套 fibhub 部署，用于试用或演示：多个 server、pod 和
# client，每个 pod 和 client 都连接所有 server，全部在 127.0.0.1 上。
#
#   ./demo.sh [start] [-s server数] [-p pod数] [-c client数] [--open]
#   ./demo.sh stop
#   ./demo.sh status
#
# start（默认）从本仓库构建 fibhub，为每个实例生成带新随机密码的配置，停掉正在
# 运行的演示后启动所有实例，然后打印每个 web 页面及其登录信息。默认 2 个
# server、2 个 pod、2 个 client（每种 1-9 个）。--open 在浏览器中打开第一个
# client 的页面；它列出每个 server 的页面（经由其 web proxy）和每个 pod 的页面
# （经由 forward），并能免登录打开它们。
#
# 每个 pod 把自己的 web 页面作为 node "web"、把 127.0.0.1:22 作为 node "ssh"；
# 每个 client 为每个 pod 转发这两个 node。pod 和 client 经 TCP 连接奇数号
# server，经 WebSocket 连接偶数号 server，消息体均加密。经 client 的 forward
# 打开 pod 的页面，各页面上就会出现 tunnel。
#
# 环境变量：DEMO_DIR（默认为本脚本旁的 .demo）存放二进制、配置、日志和 pid
# 文件；DEMO_PORT（默认 20000）为起始端口：server 用 DEMO_PORT+10*i..，pod 用
# DEMO_PORT+100+i，client 用 DEMO_PORT+200+50*i..；FIBHUB_BIN 指定使用该二进制
# 而不构建。
set -eu

die() {
	echo "demo: $*" >&2
	exit 1
}

root=$(cd "$(dirname "$0")" && pwd)
dir=${DEMO_DIR:-"$root/.demo"}
base=${DEMO_PORT:-20000}
[[ $base =~ ^[0-9]+$ ]] && ((base >= 1024 && base <= 65000)) || die "DEMO_PORT must be 1024-65000"

POD_NAMES=(lab office home factory store farm depot garage studio)
CLIENT_NAMES=(alice bob carol dave erin frank grace heidi ivan)
WEB_USER=admin

rand() {
	LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom 2>/dev/null | head -c 16
}

# up succeeds once something listens on 127.0.0.1:$1.
up() {
	(exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
}

# alive succeeds if pid file $1 names a running fibhub.
alive() {
	local pid
	pid=$(cat "$1" 2>/dev/null) || return 1
	[[ -n $pid ]] && kill -0 "$pid" 2>/dev/null && ps -p "$pid" -o command= 2>/dev/null | grep -q fibhub
}

stop() {
	local f pids=() pid
	for f in "$dir"/run/*.pid; do
		[[ -e $f ]] || continue
		alive "$f" && pids+=("$(cat "$f")")
		rm -f "$f"
	done
	((${#pids[@]} > 0)) || return 0
	echo "stopping ${#pids[@]} instances"
	kill "${pids[@]}" 2>/dev/null || true
	for _ in $(seq 1 20); do
		for pid in "${pids[@]}"; do
			kill -0 "$pid" 2>/dev/null && continue 2
		done
		return 0
	done
	kill -9 "${pids[@]}" 2>/dev/null || true
}

status() {
	[[ -f $dir/summary.txt ]] || die "no demo in $dir; run $0 start"
	local f n=0 bad=0
	for f in "$dir"/run/*.pid; do
		[[ -e $f ]] || continue
		n=$((n + 1))
		if alive "$f"; then
			echo "running  $(basename "$f" .pid)  pid $(cat "$f")"
		else
			echo "STOPPED  $(basename "$f" .pid)  (log: $dir/log/$(basename "$f" .pid).log)"
			bad=$((bad + 1))
		fi
	done
	((n > 0)) || echo "nothing running"
	echo
	cat "$dir/summary.txt"
	((bad == 0))
}

cmd=start
case ${1:-} in
start | stop | status) cmd=$1 && shift ;;
esac
nservers=2 npods=2 nclients=2 open=0
while (($# > 0)); do
	case $1 in
	-s) nservers=${2:-} && shift ;;
	-p) npods=${2:-} && shift ;;
	-c) nclients=${2:-} && shift ;;
	--open) open=1 ;;
	-h | --help) sed -n '2,/^set -eu/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//' && exit 0 ;;
	*) die "unknown argument $1 (see $0 --help)" ;;
	esac
	shift
done

case $cmd in
stop)
	stop
	exit 0
	;;
status)
	status
	exit
	;;
esac

for n in "$nservers" "$npods" "$nclients"; do
	[[ $n =~ ^[1-9]$ ]] || die "counts must be 1-9"
done

mkdir -p "$dir/conf" "$dir/log" "$dir/run"
stop
rm -f "$dir"/conf/*.json "$dir"/log/*.log

bin=${FIBHUB_BIN:-}
if [[ -z $bin ]]; then
	echo "building fibhub"
	(cd "$root" && go build -o "$dir/fibhub" .) || die "go build failed"
	bin="$dir/fibhub"
fi
[[ -x $bin ]] || die "no fibhub binary $bin"

# ---- configs / 配置 ---------------------------------------------------------

umask 077
s_name=() s_tcp=() s_ws=() s_web=() s_wsp=() s_pw=() s_body=() s_wspw=() s_auth=()
for ((i = 0; i < nservers; i++)); do
	p=$((base + 10 * i))
	s_name+=("hub-$((i + 1))")
	s_tcp+=("$p") s_ws+=("$((p + 1))") s_web+=("$((p + 2))") s_wsp+=("$((p + 3))")
	s_pw+=("$(rand)") s_body+=("$(rand)") s_wspw+=("$(rand)") s_auth+=("$WEB_USER:$(rand)")
done
p_name=() p_web=() p_auth=()
for ((i = 0; i < npods; i++)); do
	p_name+=("${POD_NAMES[$i]}") p_web+=("$((base + 100 + i))") p_auth+=("$WEB_USER:$(rand)")
done
c_name=() c_web=()
for ((i = 0; i < nclients; i++)); do
	c_name+=("${CLIENT_NAMES[$i]}") c_web+=("$((base + 200 + 50 * i))")
done

# servers_json prints the "servers" of a pod or client: odd-numbered servers
# over TCP, even-numbered over WebSocket.
servers_json() {
	local i addr sep=""
	echo '  "servers": ['
	for i in "${!s_name[@]}"; do
		if ((i % 2 == 0)); then
			addr="127.0.0.1:${s_tcp[$i]}"
		else
			addr="ws://127.0.0.1:${s_ws[$i]}/fibhub"
		fi
		printf '%s    {"name": "%s", "addr": "%s", "password": "%s", "bodyPassword": "%s"}' \
			"$sep" "${s_name[$i]}" "$addr" "${s_pw[$i]}" "${s_body[$i]}"
		sep=$',\n'
	done
	printf '\n  ],\n'
}

for i in "${!s_name[@]}"; do
	cat >"$dir/conf/server-${s_name[$i]}.json" <<EOF
{
  "mode": "server",
  "name": "${s_name[$i]}",
  "addr": "127.0.0.1:${s_tcp[$i]}",
  "arpcWsAddr": "127.0.0.1:${s_ws[$i]}",
  "arpcWsPath": "/fibhub",
  "webAddr": "127.0.0.1:${s_web[$i]}",
  "webAuth": "${s_auth[$i]}",
  "wsAddr": "127.0.0.1:${s_wsp[$i]}",
  "wsPassword": "${s_wspw[$i]}",
  "password": "${s_pw[$i]}",
  "bodyPassword": "${s_body[$i]}"
}
EOF
done

for i in "${!p_name[@]}"; do
	{
		cat <<EOF
{
  "mode": "pod",
  "name": "${p_name[$i]}",
  "webAddr": "127.0.0.1:${p_web[$i]}",
  "webAuth": "${p_auth[$i]}",
EOF
		servers_json
		cat <<EOF
  "nodes": [
    {"name": "web", "addr": "127.0.0.1:${p_web[$i]}", "desc": "web page of pod ${p_name[$i]}"},
    {"name": "ssh", "addr": "127.0.0.1:22", "desc": "SSH of this machine"}
  ],
  "meta": {"site": "${p_name[$i]}", "demo": "true"}
}
EOF
	} >"$dir/conf/pod-${p_name[$i]}.json"
done

for i in "${!c_name[@]}"; do
	port=$((c_web[i] + 1))
	{
		cat <<EOF
{
  "mode": "client",
  "name": "${c_name[$i]}",
  "webAddr": "127.0.0.1:${c_web[$i]}",
  "webAuth": "",
EOF
		servers_json
		echo '  "webProxies": ['
		sep=""
		for j in "${!s_name[@]}"; do
			printf '%s    {"name": "%s", "listen": "127.0.0.1:%s", "url": "ws://127.0.0.1:%s/", "password": "%s", "webAuth": "%s"}' \
				"$sep" "${s_name[$j]}" "$port" "${s_wsp[$j]}" "${s_wspw[$j]}" "${s_auth[$j]}"
			sep=$',\n'
			port=$((port + 1))
		done
		printf '\n  ],\n'
		echo '  "forwards": ['
		sep=""
		for j in "${!p_name[@]}"; do
			printf '%s    {"listen": "127.0.0.1:%s", "pod": "%s", "node": "web", "webAuth": "%s"},\n' \
				"$sep" "$port" "${p_name[$j]}" "${p_auth[$j]}"
			printf '    {"listen": "127.0.0.1:%s", "pod": "%s", "node": "ssh"}' "$((port + 1))" "${p_name[$j]}"
			sep=$',\n'
			port=$((port + 2))
		done
		printf '\n  ],\n'
		echo "  \"meta\": {\"owner\": \"${c_name[$i]}\", \"demo\": \"true\"}"
		echo '}'
	} >"$dir/conf/client-${c_name[$i]}.json"
done

# ---- start / 启动 -----------------------------------------------------------

# start runs the instance $1 (its config's base name) and waits for its web
# page on port $2.
start() {
	local name=$1 port=$2 pid
	nohup "$bin" -config="$dir/conf/$name.json" >"$dir/log/$name.log" 2>&1 &
	pid=$!
	echo "$pid" >"$dir/run/$name.pid"
	for _ in $(seq 1 50); do
		up "$port" && return 0
		kill -0 "$pid" 2>/dev/null || break
		sleep 0.1
	done
	echo "demo: $name did not start; the end of $dir/log/$name.log:" >&2
	tail -n 20 "$dir/log/$name.log" >&2
	stop
	exit 1
}

for p in "${s_tcp[@]}" "${s_ws[@]}" "${s_web[@]}" "${s_wsp[@]}" "${p_web[@]}" "${c_web[@]}"; do
	up "$p" && die "port $p is in use; set DEMO_PORT to another range"
done
for i in "${!s_name[@]}"; do start "server-${s_name[$i]}" "${s_web[$i]}"; done
for i in "${!p_name[@]}"; do start "pod-${p_name[$i]}" "${p_web[$i]}"; done
for i in "${!c_name[@]}"; do start "client-${c_name[$i]}" "${c_web[$i]}"; done

# ---- summary / 汇总 ---------------------------------------------------------

{
	echo "fibhub demo: ${#s_name[@]} servers, ${#p_name[@]} pods, ${#c_name[@]} clients (in $dir)"
	echo
	echo "Clients (no login; each opens every page below logged in):"
	for i in "${!c_name[@]}"; do
		printf '  %-8s http://127.0.0.1:%s\n' "${c_name[$i]}" "${c_web[$i]}"
	done
	echo "Servers:"
	for i in "${!s_name[@]}"; do
		printf '  %-8s http://127.0.0.1:%s   %s\n' "${s_name[$i]}" "${s_web[$i]}" "${s_auth[$i]}"
	done
	echo "Pods:"
	for i in "${!p_name[@]}"; do
		printf '  %-8s http://127.0.0.1:%s   %s\n' "${p_name[$i]}" "${p_web[$i]}" "${p_auth[$i]}"
	done
	echo
	echo "Theme (System, Light, Dark, Ghibli, Mecha) and language (System, English, 中文): top right of any page."
	echo "Stop: $0 stop    Status: $0 status    Logs: $dir/log/"
} >"$dir/summary.txt"
echo
cat "$dir/summary.txt"

if ((open)); then
	url="http://127.0.0.1:${c_web[0]}/"
	if command -v open >/dev/null; then
		open "$url"
	elif command -v xdg-open >/dev/null; then
		xdg-open "$url" >/dev/null 2>&1 &
	fi
fi
