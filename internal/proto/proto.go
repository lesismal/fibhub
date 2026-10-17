// Package proto is the protocol fibhub's server, pods and clients speak to
// each other over arpc: the methods, the messages they carry, the
// password check of a registration and the frames of a tunnel.
//
// A tunnel is a TCP connection a client accepted, carried to a pod through
// the server, and the TCP connection the pod opened to a node for it. Its
// data, acknowledgements and close travel as notifies whose body is a frame:
// the tunnel's ID, 8 bytes big-endian, and then the payload. The server
// passes a frame on as it is, to the other end of the tunnel.
package proto

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// Version is the fibhub version every peer reports.
const Version = "0.1.0"

// The types of peer a server serves.
const (
	TypePod    = "pod"
	TypeClient = "client"
)

// The methods. Those a peer calls on the server start with /fibhub/, those
// the server calls on a pod with /pod/, and the tunnel methods go both ways.
const (
	// MethodRegister authenticates a pod or client and registers it:
	// RegisterReq answered with RegisterRsp. It must be the first call on a
	// connection; the server refuses every other one until it succeeds.
	MethodRegister = "/fibhub/register"
	// MethodUpdate replaces what a registered peer told about itself: Info
	// answered with "ok".
	MethodUpdate = "/fibhub/update"
	// MethodPods lists the pods a server has: an empty request answered with
	// PodList. The server also notifies it, with a PodList, to every client
	// whenever the list changes.
	MethodPods = "/fibhub/pods"
	// MethodSetPodNodes has the server set a pod's nodes: SetNodesReq
	// answered with the pod's new Info.
	MethodSetPodNodes = "/fibhub/pod/nodes"
	// MethodKick tells a peer the server is closing its connection, with the
	// reason as the body.
	MethodKick = "/fibhub/kick"

	// MethodPodInfo asks a pod for its Info: an empty request answered with
	// Info.
	MethodPodInfo = "/pod/info"
	// MethodPodSetNodes sets a pod's nodes: []Node answered with the pod's
	// new Info.
	MethodPodSetNodes = "/pod/nodes"

	// MethodTunnelOpen opens a tunnel: a client calls it on the server with
	// a TunnelOpenReq, and the server calls it on the pod with the same
	// request, Client filled in. Both answer with a TunnelOpenRsp.
	MethodTunnelOpen = "/tunnel/open"
	// MethodTunnelData carries a frame of the tunnel's bytes.
	MethodTunnelData = "/tunnel/data"
	// MethodTunnelAck acknowledges bytes of a tunnel its end has written to
	// its TCP connection: a frame whose payload is the count, 8 bytes
	// big-endian. An end sends no more than Window bytes it has not had
	// acknowledged.
	MethodTunnelAck = "/tunnel/ack"
	// MethodTunnelClose closes a tunnel: a frame whose payload is the
	// reason. An end sends it after the last of its data, and the other end
	// closes its TCP connection once that data is written.
	MethodTunnelClose = "/tunnel/close"
)

// Flow control of a tunnel.
const (
	// Window is how many bytes one end of a tunnel may send that the other
	// has not acknowledged.
	Window = 4 << 20
	// AckEvery is how many bytes an end lets arrive before it acknowledges
	// them.
	AckEvery = 256 << 10
	// MaxChunk is the most bytes one data frame carries.
	MaxChunk = 64 << 10
)

// Node is a device a pod can reach.
type Node struct {
	// Name identifies the node among its pod's.
	Name string `json:"name"`
	// Addr is the node's TCP address, host:port.
	Addr string `json:"addr"`
	Desc string `json:"desc,omitempty"`
}

// Info is what a peer tells the server about itself.
type Info struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Version  string `json:"version,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	OS       string `json:"os,omitempty"`
	// Nodes are the nodes a pod can reach.
	Nodes []Node `json:"nodes,omitempty"`
	// Meta is anything else the peer was configured to tell.
	Meta map[string]string `json:"meta,omitempty"`
}

// NewInfo is an Info of this process.
func NewInfo(name, typ string) Info {
	host, _ := os.Hostname()
	return Info{Name: name, Type: typ, Version: Version, Hostname: host, OS: runtime.GOOS + "/" + runtime.GOARCH}
}

// FindNode returns the node of info named name.
func (info *Info) FindNode(name string) (Node, bool) {
	for _, n := range info.Nodes {
		if n.Name == name {
			return n, true
		}
	}
	return Node{}, false
}

// RegisterReq authenticates and registers a peer. MAC is the HMAC-SHA256,
// keyed with the server's password, of the peer's type and name, Time and
// Nonce; see Sign.
type RegisterReq struct {
	Info  Info   `json:"info"`
	Time  int64  `json:"time"`
	Nonce string `json:"nonce"`
	MAC   string `json:"mac"`
}

// RegisterRsp answers a registration.
type RegisterRsp struct {
	// ID is the peer's number on the server for as long as its connection
	// lasts. The tunnels a client opens have it in their IDs' high 32 bits.
	ID uint32 `json:"id"`
	// Server is the server's name.
	Server string `json:"server"`
}

// PodView is a pod as the server lists it to clients.
type PodView struct {
	Info  Info      `json:"info"`
	Addr  string    `json:"addr"`
	Since time.Time `json:"since"`
}

// PodList is the pods of a server.
type PodList struct {
	Server string    `json:"server"`
	Pods   []PodView `json:"pods"`
}

// SetNodesReq sets the nodes of the pod named Pod.
type SetNodesReq struct {
	Pod   string `json:"pod"`
	Nodes []Node `json:"nodes"`
}

// TunnelOpenReq opens tunnel ID to node Node of pod Pod.
type TunnelOpenReq struct {
	ID   uint64 `json:"id"`
	Pod  string `json:"pod"`
	Node string `json:"node"`
	// Client is the name of the client that opens it, which the server
	// fills in for the pod.
	Client string `json:"client,omitempty"`
}

// TunnelOpenRsp answers a TunnelOpenReq once the pod is connected to the
// node.
type TunnelOpenRsp struct {
	ID uint64 `json:"id"`
	// Addr is the node's address.
	Addr string `json:"addr"`
}

// TunnelID is the ID of the seq-th tunnel of the peer the server numbered
// peer.
func TunnelID(peer uint32, seq uint32) uint64 { return uint64(peer)<<32 | uint64(seq) }

// TunnelPeer is the peer number in a tunnel's ID.
func TunnelPeer(id uint64) uint32 { return uint32(id >> 32) }

// Frame is a tunnel frame: id and then payload.
func Frame(id uint64, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint64(b, id)
	copy(b[8:], payload)
	return b
}

// AckFrame is the frame acknowledging n bytes of tunnel id.
func AckFrame(id uint64, n int64) []byte {
	b := make([]byte, 16)
	binary.BigEndian.PutUint64(b, id)
	binary.BigEndian.PutUint64(b[8:], uint64(n))
	return b
}

// ErrShortFrame is the error of a frame too short to carry a tunnel ID.
var ErrShortFrame = errors.New("fibhub: frame shorter than a tunnel ID")

// ParseFrame splits a frame into its tunnel ID and payload.
func ParseFrame(b []byte) (uint64, []byte, error) {
	if len(b) < 8 {
		return 0, nil, ErrShortFrame
	}
	return binary.BigEndian.Uint64(b), b[8:], nil
}

// ParseAck is the count of an acknowledgement's payload.
func ParseAck(payload []byte) (int64, error) {
	if len(payload) != 8 {
		return 0, fmt.Errorf("fibhub: ack payload of %d bytes", len(payload))
	}
	return int64(binary.BigEndian.Uint64(payload)), nil
}

// MaxClockSkew is how far a registration's Time may be from the server's
// clock.
const MaxClockSkew = 5 * time.Minute

// Sign fills in req's Time, Nonce and MAC for password.
func Sign(req *RegisterReq, password string) {
	var nonce [16]byte
	_, _ = rand.Read(nonce[:])
	req.Time = time.Now().Unix()
	req.Nonce = hex.EncodeToString(nonce[:])
	req.MAC = mac(req, password)
}

func mac(req *RegisterReq, password string) string {
	h := hmac.New(sha256.New, []byte(password))
	h.Write([]byte(req.Info.Type + "\n" + req.Info.Name + "\n" + strconv.FormatInt(req.Time, 10) + "\n" + req.Nonce))
	return hex.EncodeToString(h.Sum(nil))
}

// Verifier checks registrations against a password, and refuses a nonce it
// has seen within MaxClockSkew, so a registration overheard cannot be
// replayed.
type Verifier struct {
	Password string
	mu       sync.Mutex
	seen     map[string]time.Time
}

// Verify checks req.
func (v *Verifier) Verify(req *RegisterReq) error {
	if req.Info.Name == "" {
		return errors.New("fibhub: a peer needs a name")
	}
	if req.Info.Type != TypePod && req.Info.Type != TypeClient {
		return fmt.Errorf("fibhub: unknown peer type %q", req.Info.Type)
	}
	now := time.Now()
	if d := now.Sub(time.Unix(req.Time, 0)); d > MaxClockSkew || d < -MaxClockSkew {
		return errors.New("fibhub: registration time too far from the server's clock")
	}
	want := mac(req, v.Password)
	if len(req.Nonce) < 16 || !hmac.Equal([]byte(want), []byte(req.MAC)) {
		return errors.New("fibhub: authentication failed")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.seen == nil {
		v.seen = map[string]time.Time{}
	}
	for nonce, at := range v.seen {
		if now.Sub(at) > 2*MaxClockSkew {
			delete(v.seen, nonce)
		}
	}
	if _, ok := v.seen[req.Nonce]; ok {
		return errors.New("fibhub: registration replayed")
	}
	v.seen[req.Nonce] = now
	return nil
}
