#!/usr/bin/env bash
# gen-configs.sh writes a set of fibhub config files for the servers, pods
# and clients that config.sh, next to it, names. Every password is random;
# each pod and client lists every server with that server's own password
# and bodyPassword.
#
# gen-configs.sh 为旁边 config.sh 中列出的 server、pod 和 client 生成一套
# fibhub 配置文件。所有密码都是随机的；每个 pod 和 client 都列出所有 server，
# 各自带有该 server 自己的 password 和 bodyPassword。
#
#   ./configs/gen-configs.sh [out-dir]     # out-dir defaults to configs/gen
#                                          # out-dir 默认为 configs/gen
#
# Ports (instances of one mode run on different hosts, so each uses the same
# ports):
#
# 端口（同一模式的实例运行在不同主机上，所以都使用相同的端口）：
#
#   server  10000 addr, 10001 arpcWsAddr, 10002 webAddr (127.0.0.1 only),
#           10003 wsAddr (10004-10005 spare)
#   pod     10006 webAddr (10007-10010 spare)
#   client  10011 webAddr, 10012-10015 webProxies then forwards
#
#   server  10000 addr，10001 arpcWsAddr，10002 webAddr（仅 127.0.0.1），
#           10003 wsAddr（10004-10005 备用）
#   pod     10006 webAddr（10007-10010 备用）
#   client  10011 webAddr，10012-10015 先给 webProxies、再给 forwards
#
# Each client gets a webProxy per server: a 127.0.0.1 port that serves that
# server's web page through its wsAddr, encrypted with its wsPassword. The
# webProxies, and the forwards of nodes that are a pod's own web page, carry
# that page's webAuth, so the client's web page shows it and opens the page
# logged in.
#
# 每个 client 为每个 server 得到一个 webProxy：一个 127.0.0.1 端口，经由该
# server 的 wsAddr 提供它的 web 页面，用它的 wsPassword 加密。webProxies，以及
# 指向 pod 自身 web 页面的 node 的 forward，都带有该页面的 webAuth，因此 client
# 的 web 页面会显示它，并能免登录打开该页面。
set -eu

die() {
	echo "gen-configs: $*" >&2
	exit 1
}

# ---- settings / 设置 --------------------------------------------------------

# SERVERS, PODS and CLIENTS come from config.sh.
# SERVERS、PODS 和 CLIENTS 来自 config.sh。
config="$(cd "$(dirname "$0")" && pwd)/config.sh"
[[ -f $config ]] || die "no $config"
# shellcheck source=config.sh
. "$config"
for v in SERVERS PODS CLIENTS; do
	declare -p "$v" 2>/dev/null | grep -q '^declare -a' || die "$config does not set the array $v"
done

# How pods and clients reach a server: "ws" (ws://ip:arpcWsAddr/fibhub, for
# networks that let only HTTP through) or "tcp" (ip:addr).
# pod 和 client 连接 server 的方式："ws"（ws://ip:arpcWsAddr/fibhub，适用于只
# 放行 HTTP 的网络）或 "tcp"（ip:addr）。
UPLINK="ws"

# Nodes every pod offers; NODE_ADDRS[i] is NODES[i]'s address. A "{web}" in
# an address becomes the pod's own web port.
# 每个 pod 提供的 node；NODE_ADDRS[i] 是 NODES[i] 的地址。地址中的 "{web}" 会
# 被替换为 pod 自己的 web 端口。
NODES=("ssh" "web")
NODE_ADDRS=("127.0.0.1:22" "127.0.0.1:{web}")

# Nodes each client forwards for every pod. A client's webProxies, one per
# server, and then its forwards take ports from CLIENT_PORTS in order; any
# beyond the last port are skipped.
# 每个 client 为每个 pod 转发的 node。client 的 webProxies（每个 server 一个）
# 和随后的 forwards 依次从 CLIENT_PORTS 取端口；超出最后一个端口的会被跳过。
FORWARD_NODES=("ssh" "web")

# Username of every web page's basic auth.
# 所有 web 页面 basic auth 的用户名。
WEB_USER="admin"

# Length of the random passwords.
# 随机密码的长度。
PASSWORD_LEN=24

SERVER_ADDR_PORT=10000
SERVER_ARPC_WS_PORT=10001
SERVER_WEB_PORT=10002
SERVER_WS_PORT=10003
SERVER_ARPC_WS_PATH="/fibhub"
POD_WEB_PORT=10006
CLIENT_WEB_PORT=10011
CLIENT_PORTS=(10012 10013 10014 10015)

# ---- generation / 生成 ------------------------------------------------------

out_dir=${1:-"$(cd "$(dirname "$0")" && pwd)/gen"}

# rand prints a random alphanumeric password.
# rand 输出一个由字母和数字组成的随机密码。
rand() {
	LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom 2>/dev/null | head -c "$PASSWORD_LEN"
}

# check_names fails unless every argument is a usable, distinct name.
# check_names 在任一参数不是合法且不重复的名字时报错退出。
check_names() {
	local kind=$1 seen=" " n
	shift
	for n in "$@"; do
		[[ $n =~ ^[A-Za-z0-9._-]+$ ]] || die "$kind name \"$n\" may use only letters, digits, '.', '_' and '-'"
		[[ $seen == *" $n "* ]] && die "$kind name \"$n\" is used twice"
		seen+="$n "
	done
}

server_names=() server_ips=()
for s in "${SERVERS[@]}"; do
	read -r name ip extra <<<"$s"
	[[ -n ${ip:-} && -z ${extra:-} ]] || die "server \"$s\" is not \"name ip\""
	[[ $ip =~ ^[A-Za-z0-9.:_-]+$ ]] || die "server $name has a bad ip \"$ip\""
	server_names+=("$name")
	server_ips+=("$ip")
done
((${#server_names[@]} > 0)) || die "SERVERS is empty"
check_names server "${server_names[@]}"
check_names pod "${PODS[@]}"
check_names client "${CLIENTS[@]}"
((${#NODES[@]} == ${#NODE_ADDRS[@]})) || die "NODES and NODE_ADDRS differ in length"
[[ $UPLINK == ws || $UPLINK == tcp ]] || die "UPLINK must be ws or tcp"

# The servers' secrets, which the pods and clients share, and the web pages'
# logins, which the clients know.
# server 的密钥（pod 和 client 共享）以及各 web 页面的登录信息（client 知道）。
server_pw=() server_body_pw=() server_ws_pw=() server_web_auth=()
for i in "${!server_names[@]}"; do
	server_pw+=("$(rand)")
	server_body_pw+=("$(rand)")
	server_ws_pw+=("$(rand)")
	server_web_auth+=("$WEB_USER:$(rand)")
done
pod_web_auth=()
for pod in "${PODS[@]}"; do
	pod_web_auth+=("$WEB_USER:$(rand)")
done

# node_is_web succeeds if the node named $1 is the pod's own web page.
# 若名为 $1 的 node 是 pod 自身的 web 页面，node_is_web 返回成功。
node_is_web() {
	local j
	for j in "${!NODES[@]}"; do
		[[ ${NODES[$j]} == "$1" && ${NODE_ADDRS[$j]} == *"{web}"* ]] && return 0
	done
	return 1
}

mkdir -p "$out_dir"
umask 077

# servers_json prints the "servers" array of a pod or client.
# servers_json 输出 pod 或 client 的 "servers" 数组。
servers_json() {
	local i addr sep=""
	echo '  "servers": ['
	for i in "${!server_names[@]}"; do
		if [[ $UPLINK == ws ]]; then
			addr="ws://${server_ips[$i]}:$SERVER_ARPC_WS_PORT$SERVER_ARPC_WS_PATH"
		else
			addr="${server_ips[$i]}:$SERVER_ADDR_PORT"
		fi
		printf '%s    {\n      "name": "%s",\n      "addr": "%s",\n      "password": "%s",\n      "bodyPassword": "%s"\n    }' \
			"$sep" "${server_names[$i]}" "$addr" "${server_pw[$i]}" "${server_body_pw[$i]}"
		sep=$',\n'
	done
	printf '\n  ],\n'
}

for i in "${!server_names[@]}"; do
	f="$out_dir/server-${server_names[$i]}.json"
	cat >"$f" <<EOF
{
  "mode": "server",
  "name": "${server_names[$i]}",
  "addr": ":$SERVER_ADDR_PORT",
  "arpcWsAddr": ":$SERVER_ARPC_WS_PORT",
  "arpcWsPath": "$SERVER_ARPC_WS_PATH",
  "webAddr": "127.0.0.1:$SERVER_WEB_PORT",
  "webAuth": "${server_web_auth[$i]}",
  "wsAddr": ":$SERVER_WS_PORT",
  "wsPassword": "${server_ws_pw[$i]}",
  "password": "${server_pw[$i]}",
  "bodyPassword": "${server_body_pw[$i]}"
}
EOF
	echo "$f"
done

for p in "${!PODS[@]}"; do
	pod=${PODS[$p]}
	f="$out_dir/pod-$pod.json"
	{
		cat <<EOF
{
  "mode": "pod",
  "name": "$pod",
  "webAddr": ":$POD_WEB_PORT",
  "webAuth": "${pod_web_auth[$p]}",
EOF
		servers_json
		echo '  "nodes": ['
		sep=""
		for j in "${!NODES[@]}"; do
			printf '%s    {\n      "name": "%s",\n      "addr": "%s"\n    }' \
				"$sep" "${NODES[$j]}" "${NODE_ADDRS[$j]//\{web\}/$POD_WEB_PORT}"
			sep=$',\n'
		done
		printf '\n  ]\n}\n'
	} >"$f"
	echo "$f"
done

for client in "${CLIENTS[@]}"; do
	f="$out_dir/client-$client.json"
	{
		cat <<EOF
{
  "mode": "client",
  "name": "$client",
  "webAddr": "127.0.0.1:$CLIENT_WEB_PORT",
  "webAuth": "$WEB_USER:$(rand)",
EOF
		servers_json
		echo '  "webProxies": ['
		sep="" k=0
		for i in "${!server_names[@]}"; do
			if ((k >= ${#CLIENT_PORTS[@]})); then
				echo "gen-configs: $client: no port left for the web proxy of ${server_names[$i]}, skipped" >&2
				continue
			fi
			printf '%s    {\n      "name": "%s",\n      "listen": "127.0.0.1:%s",\n      "url": "ws://%s:%s/",\n      "password": "%s",\n      "webAuth": "%s"\n    }' \
				"$sep" "${server_names[$i]}" "${CLIENT_PORTS[$k]}" "${server_ips[$i]}" "$SERVER_WS_PORT" "${server_ws_pw[$i]}" "${server_web_auth[$i]}"
			sep=$',\n'
			k=$((k + 1))
		done
		printf '\n  ],\n'
		echo '  "forwards": ['
		sep=""
		for p in "${!PODS[@]}"; do
			pod=${PODS[$p]}
			for node in "${FORWARD_NODES[@]}"; do
				if ((k >= ${#CLIENT_PORTS[@]})); then
					echo "gen-configs: $client: no port left to forward $pod/$node, skipped" >&2
					continue
				fi
				web=""
				if node_is_web "$node"; then
					web=$(printf ',\n      "webAuth": "%s"' "${pod_web_auth[$p]}")
				fi
				printf '%s    {\n      "listen": "127.0.0.1:%s",\n      "pod": "%s",\n      "node": "%s"%s\n    }' \
					"$sep" "${CLIENT_PORTS[$k]}" "$pod" "$node" "$web"
				sep=$',\n'
				k=$((k + 1))
			done
		done
		printf '\n  ]\n}\n'
	} >"$f"
	echo "$f"
done
