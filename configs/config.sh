# config.sh names the fibhub servers, pods and clients that gen-configs.sh
# writes config files for. gen-configs.sh sources it: it is bash, and each
# list is a bash array with one quoted entry per line. Edit the lists at the
# bottom, then run
#
# config.sh 列出 gen-configs.sh 要为之生成配置文件的 fibhub server、pod 和
# client。gen-configs.sh 会 source 这个文件：它就是 bash，每个列表是一个 bash
# 数组，每行一个带引号的条目。修改下面的列表，然后运行
#
#   ./configs/gen-configs.sh [out-dir]     # out-dir defaults to configs/gen
#                                          # out-dir 默认为 configs/gen
#
# which writes one file per entry into out-dir:
#
# 它会为每个条目在 out-dir 中写一个文件：
#
#   server-<name>.json   pod-<name>.json   client-<name>.json
#
# with every password derived from the key seed it asks for: the same seed
# gives the same passwords on every run, and an empty one takes a random
# seed, so the run's files must be deployed together. Every pod and every
# client gets all
# the servers listed here, each with that server's own passwords; every
# client gets a web proxy for each server's web page and forwards to the
# nodes of every pod (gen-configs.sh says which nodes and ports).
#
# 所有密码都由它要求输入的密钥种子派生：同一个种子每次运行得到相同的密码，留空
# 则取随机种子，此时这次运行生成的文件必须一起部署。每个 pod 和每个 client 都会得到这里列出的所有 server，各自带有该 server
# 自己的密码；每个 client 会为每个 server 的 web 页面得到一个 web proxy，并为
# 每个 pod 的 node 得到 forward（具体哪些 node、哪些端口见 gen-configs.sh）。
#
# Names may use letters, digits, '.', '_' and '-', and must differ within a
# list. They show on the web pages, and a client's forwards name pods by
# their names.
#
# 名字只能使用字母、数字、'.'、'_' 和 '-'，同一列表内不能重复。名字会显示在
# web 页面上，client 的 forward 也是按名字指定 pod 的。
#
# Ports: every instance of one mode uses the same ports (set in
# gen-configs.sh: server 10000-10003, pod 10006, client 10011-10015), so the
# instances of one mode are meant to run on different hosts, and restart.sh
# stops every running process of a mode before it starts that mode's config.
# Lists with several 127.0.0.1 entries, like the examples below, generate
# fine and are templates: replace 127.0.0.1 with each host's address before
# deploying, or give the instances that share a host other ports by hand and
# start them with `fibhub -config=<file>`.
#
# 端口：同一模式的所有实例使用相同的端口（在 gen-configs.sh 中设置：server
# 10000-10003，pod 10006，client 10011-10015），所以同一模式的实例应运行在不同
# 主机上；restart.sh 在启动某模式的配置前，也会先停掉该模式所有正在运行的进程。
# 像下面示例那样含多个 127.0.0.1 条目的列表可以正常生成，但只是模板：部署前把
# 127.0.0.1 换成各主机的地址，或者手动给同一主机上的实例改用其他端口，并用
# `fibhub -config=<文件>` 启动它们。
#
# A client takes a port for each server's web proxy, and then one for each
# pod's forwarded node, from CLIENT_PORTS in gen-configs.sh (4 ports): with
# N servers, P pods and F forwarded nodes it needs N + P*F of them, and
# skips (with a warning) whatever does not fit. Add ports there for more.
#
# client 依次从 gen-configs.sh 的 CLIENT_PORTS（4 个端口）中取端口：先为每个
# server 的 web proxy 各取一个，再为每个 pod 的每个转发 node 各取一个：N 个
# server、P 个 pod、F 个转发 node 共需要 N + P*F 个端口，放不下的会被跳过（并
# 给出警告）。需要更多时请在那里添加端口。

# ---- SERVERS ----------------------------------------------------------------
#
# "name ip" for each fibhub server. ip is the address pods and clients reach
# that server at, an IP or a host name: it goes in their "servers" addresses
# (ws://ip:10001/fibhub, or ip:10000 with UPLINK="tcp" in gen-configs.sh)
# and in the clients' web proxy URLs (ws://ip:10003/).
#
# 每个 fibhub server 一条 "名字 ip"。ip 是 pod 和 client 连接该 server 所用的
# 地址，可以是 IP 或主机名：它会写进它们的 "servers" 地址（ws://ip:10001/fibhub，
# 若 gen-configs.sh 中 UPLINK="tcp" 则为 ip:10000），以及 client 的 web proxy
# URL（ws://ip:10003/）。
#
# Examples / 示例:
#
#   One server, everything on this machine, to try fibhub out:
#   一个 server，全部跑在本机上，用来试用 fibhub：
#     SERVERS=(
#       "server 127.0.0.1"
#     )
#
#   Three servers (replace each 127.0.0.1 with that server's address):
#   三个 server（把每个 127.0.0.1 换成该 server 的地址）：
#     SERVERS=(
#       "hub-1 127.0.0.1"
#       "hub-2 127.0.0.1"
#       "hub-3 127.0.0.1"
#     )
#
#   Servers on real hosts:
#   部署在真实主机上的 server：
#     SERVERS=(
#       "hk 203.0.113.10"
#       "sg 198.51.100.20"
#       "office hub.example.com"
#     )
SERVERS=(
	"server 127.0.0.1"
)

# ---- PODS -------------------------------------------------------------------
#
# Name of each pod: a machine inside a network that reaches the nodes (the
# devices' TCP ports) clients connect to. A pod needs no address, it dials
# out to the servers.
#
# 每个 pod 的名字：pod 是某个内网中的一台机器，能访问 client 要连接的 node（各
# 设备的 TCP 端口）。pod 不需要地址，它主动连出到 server。
#
# Examples / 示例:
#
#   One pod:
#   一个 pod：
#     PODS=(
#       "pod-1"
#     )
#
#   One pod per site:
#   每个站点一个 pod：
#     PODS=(
#       "lab"
#       "office"
#       "home"
#     )
PODS=(
	"pod-5090"
)

# ---- CLIENTS ----------------------------------------------------------------
#
# Name of each client: the machine people work on, which serves the forwards
# on 127.0.0.1 ports. A client needs no address either.
#
# 每个 client 的名字：client 是人们日常使用的机器，在 127.0.0.1 的端口上提供
# forward。client 同样不需要地址。
#
# Examples / 示例:
#
#   One client:
#   一个 client：
#     CLIENTS=(
#       "client-1"
#     )
#
#   One client per person or laptop:
#   每人或每台电脑一个 client：
#     CLIENTS=(
#       "alice-laptop"
#       "bob-macmini"
#       "ops-desktop"
#     )
CLIENTS=(
	"client-macmini"
)
