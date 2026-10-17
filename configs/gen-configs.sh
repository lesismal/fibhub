#!/usr/bin/env bash
# gen-configs.sh writes a set of fibhub config files for the servers, pods
# and clients named below. Every password is random; each pod and client
# lists every server with that server's own password and bodyPassword.
#
#   ./configs/gen-configs.sh [out-dir]     # out-dir defaults to configs/gen
#
# Ports (instances of one mode run on different hosts, so each uses the same
# ports):
#   server  10000 addr, 10001 arpcWsAddr, 10002 webAddr (127.0.0.1 only),
#           10003 wsAddr (10004-10005 spare)
#   pod     10006 webAddr (10007-10010 spare)
#   client  10011 webAddr, 10012-10015 webProxies then forwards
#
# Each client gets a webProxy per server: a 127.0.0.1 port that serves that
# server's web page through its wsAddr, encrypted with its wsPassword.
set -eu

# ---- settings ---------------------------------------------------------------

# "name ip" of each fibhub server.
SERVERS=(
	"server 127.0.0.1"
)

# Names of the pods and clients.
PODS=(
	"pod-5090"
)
CLIENTS=(
	"client-macmini"
)

# How pods and clients reach a server: "ws" (ws://ip:arpcWsAddr/fibhub, for
# networks that let only HTTP through) or "tcp" (ip:addr).
UPLINK="ws"

# Nodes every pod offers; NODE_ADDRS[i] is NODES[i]'s address. A "{web}" in
# an address becomes the pod's own web port.
NODES=("ssh" "web")
NODE_ADDRS=("127.0.0.1:22" "127.0.0.1:{web}")

# Nodes each client forwards for every pod. A client's webProxies, one per
# server, and then its forwards take ports from CLIENT_PORTS in order; any
# beyond the last port are skipped.
FORWARD_NODES=("ssh" "web")

# Username of every web page's basic auth.
WEB_USER="admin"

# Length of the random passwords.
PASSWORD_LEN=24

SERVER_ADDR_PORT=10000
SERVER_ARPC_WS_PORT=10001
SERVER_WEB_PORT=10002
SERVER_WS_PORT=10003
SERVER_ARPC_WS_PATH="/fibhub"
POD_WEB_PORT=10006
CLIENT_WEB_PORT=10011
CLIENT_PORTS=(10012 10013 10014 10015)

# ---- generation -------------------------------------------------------------

out_dir=${1:-"$(cd "$(dirname "$0")" && pwd)/gen"}

die() {
	echo "gen-configs: $*" >&2
	exit 1
}

# rand prints a random alphanumeric password.
rand() {
	LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom 2>/dev/null | head -c "$PASSWORD_LEN"
}

# check_names fails unless every argument is a usable, distinct name.
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

# The servers' secrets, which the pods and clients share.
server_pw=() server_body_pw=() server_ws_pw=()
for i in "${!server_names[@]}"; do
	server_pw+=("$(rand)")
	server_body_pw+=("$(rand)")
	server_ws_pw+=("$(rand)")
done

mkdir -p "$out_dir"
umask 077

# servers_json prints the "servers" array of a pod or client.
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
  "webAuth": "$WEB_USER:$(rand)",
  "wsAddr": ":$SERVER_WS_PORT",
  "wsPassword": "${server_ws_pw[$i]}",
  "password": "${server_pw[$i]}",
  "bodyPassword": "${server_body_pw[$i]}"
}
EOF
	echo "$f"
done

for pod in "${PODS[@]}"; do
	f="$out_dir/pod-$pod.json"
	{
		cat <<EOF
{
  "mode": "pod",
  "name": "$pod",
  "webAddr": ":$POD_WEB_PORT",
  "webAuth": "$WEB_USER:$(rand)",
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
			printf '%s    {\n      "listen": "127.0.0.1:%s",\n      "url": "ws://%s:%s/",\n      "password": "%s"\n    }' \
				"$sep" "${CLIENT_PORTS[$k]}" "${server_ips[$i]}" "$SERVER_WS_PORT" "${server_ws_pw[$i]}"
			sep=$',\n'
			k=$((k + 1))
		done
		printf '\n  ],\n'
		echo '  "forwards": ['
		sep=""
		for pod in "${PODS[@]}"; do
			for node in "${FORWARD_NODES[@]}"; do
				if ((k >= ${#CLIENT_PORTS[@]})); then
					echo "gen-configs: $client: no port left to forward $pod/$node, skipped" >&2
					continue
				fi
				printf '%s    {\n      "listen": "127.0.0.1:%s",\n      "pod": "%s",\n      "node": "%s"\n    }' \
					"$sep" "${CLIENT_PORTS[$k]}" "$pod" "$node"
				sep=$',\n'
				k=$((k + 1))
			done
		done
		printf '\n  ]\n}\n'
	} >"$f"
	echo "$f"
done
