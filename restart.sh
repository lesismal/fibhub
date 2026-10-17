#!/usr/bin/env bash
# restart.sh (re)starts fibhub with a config file:
#
#   ./restart.sh server.json [fibhub-binary]
#
# The config's "mode" names the process, fibhub-server, fibhub-pod or
# fibhub-client. Every running process of that name is stopped, the fibhub
# binary (default: the one next to this script) is copied over that name
# next to it, and the copy is started with nohup, logging to the config's
# path with .log in place of .json.
set -eu

die() {
	echo "restart: $*" >&2
	exit 1
}

[[ $# -ge 1 && $# -le 2 ]] || die "usage: $0 config.json [fibhub-binary]"
cfg=$1
[[ -f $cfg ]] || die "no config file $cfg"
dir=$(cd "$(dirname "$0")" && pwd)
src=${2:-"$dir/fibhub"}
[[ -f $src ]] || die "no fibhub binary $src"

mode=$(sed -n 's/^[[:space:]]*"mode"[[:space:]]*:[[:space:]]*"\([a-z]*\)".*/\1/p' "$cfg" | head -n 1)
case $mode in
server | pod | client) ;;
*) die "$cfg has no \"mode\" of server, pod or client" ;;
esac
name="fibhub-$mode"
bin="$(cd "$(dirname "$src")" && pwd)/$name"
log="${cfg%.json}.log"

# Stop the running ones, giving them 10s before SIGKILL.
pids=$(pgrep -x "$name" || true)
if [[ -n $pids ]]; then
	echo "stopping $name:" $pids
	kill $pids 2>/dev/null || true
	for _ in $(seq 1 20); do
		pgrep -x "$name" >/dev/null || break
		sleep 0.5
	done
	if pids=$(pgrep -x "$name"); then
		echo "killing $name:" $pids
		kill -9 $pids 2>/dev/null || true
		sleep 0.5
	fi
fi

if [[ $src != "$bin" ]]; then
	cp -f "$src" "$bin"
	chmod +x "$bin"
fi

nohup "$bin" -config="$cfg" >"$log" 2>&1 &
pid=$!
sleep 1
if ! kill -0 "$pid" 2>/dev/null; then
	echo "$name exited; the end of $log:" >&2
	tail -n 20 "$log" >&2
	exit 1
fi
echo "$name started, pid $pid, log $log"
