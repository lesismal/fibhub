//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Package bodycrypt is an arpc MessageCoder that encrypts the body of every
// message, its method and payload, with AES-256-GCM under a key derived from
// a password both sides are configured with.
//
// An encrypted message keeps its header, with bit 0 of the reserved byte
// set, and carries a random 12-byte nonce followed by the sealed body. The
// header from the reserved byte on - the flag, the command, the flags, the
// method length and the sequence number - is the sealing's additional data,
// so it cannot be changed either. A message that arrives unencrypted, or
// does not open, is dropped and its connection closed: a peer configured
// with another password, or none, gets nowhere.
package bodycrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"log/slog"

	"github.com/lesismal/fib/arpc"
)

// flagBit is the bit of the header's reserved byte that marks an encrypted
// body.
const flagBit = 0

const (
	salt      = "fibhub-arpc-body"
	iter      = 10000
	nonceSize = 12
)

var (
	errUnencrypted = errors.New("bodycrypt: received an unencrypted message")
	errOpen        = errors.New("bodycrypt: message failed to decrypt; are the passwords the same?")
)

// Coder encrypts and decrypts message bodies.
type Coder struct {
	aead cipher.AEAD
}

// New is a Coder whose key is derived from password.
func New(password string) (*Coder, error) {
	if password == "" {
		return nil, errors.New("bodycrypt: empty password")
	}
	key, err := pbkdf2.Key(sha256.New, password, []byte(salt), iter, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Coder{aead: aead}, nil
}

// Encode seals msg's body in place.
func (cd *Coder) Encode(c *arpc.Client, msg *arpc.Message) *arpc.Message {
	head := msg.Buffer[:arpc.HeadLen]
	body := msg.Buffer[arpc.HeadLen:]
	out := c.Handler.Malloc(arpc.HeadLen + nonceSize + len(body) + cd.aead.Overhead())
	copy(out, head)
	out[arpc.HeaderIndexReserved] |= 1 << flagBit
	nonce := out[arpc.HeadLen : arpc.HeadLen+nonceSize]
	_, _ = rand.Read(nonce)
	sealed := cd.aead.Seal(out[arpc.HeadLen+nonceSize:arpc.HeadLen+nonceSize], nonce, body, out[arpc.HeaderIndexReserved:arpc.HeadLen])
	out = out[:arpc.HeadLen+nonceSize+len(sealed)]
	binary.LittleEndian.PutUint32(out[arpc.HeaderIndexBodyLenBegin:], uint32(len(out)-arpc.HeadLen))
	c.Handler.Free(msg.Buffer)
	msg.Buffer = out
	return msg
}

// Decode opens msg's body in place. A message that is not encrypted, or does
// not open, it empties of its method, which arpc drops, and closes the
// connection it came on.
func (cd *Coder) Decode(c *arpc.Client, msg *arpc.Message) *arpc.Message {
	if !msg.IsFlagBitSet(flagBit) {
		return reject(c, msg, errUnencrypted)
	}
	in := msg.Buffer
	if len(in) < arpc.HeadLen+nonceSize+cd.aead.Overhead() {
		return reject(c, msg, errOpen)
	}
	nonce := in[arpc.HeadLen : arpc.HeadLen+nonceSize]
	out := c.Handler.Malloc(len(in) - nonceSize - cd.aead.Overhead())
	copy(out, in[:arpc.HeadLen])
	body, err := cd.aead.Open(out[arpc.HeadLen:arpc.HeadLen], nonce, in[arpc.HeadLen+nonceSize:], in[arpc.HeaderIndexReserved:arpc.HeadLen])
	if err != nil {
		c.Handler.Free(out)
		return reject(c, msg, errOpen)
	}
	out = out[:arpc.HeadLen+len(body)]
	out[arpc.HeaderIndexReserved] &^= 1 << flagBit
	binary.LittleEndian.PutUint32(out[arpc.HeaderIndexBodyLenBegin:], uint32(len(body)))
	c.Handler.Free(in)
	msg.Buffer = out
	return msg
}

// reject empties msg of its method, so arpc drops it, and closes c's
// connection.
func reject(c *arpc.Client, msg *arpc.Message, err error) *arpc.Message {
	remote := ""
	if conn := c.Conn(); conn != nil {
		if a := conn.RemoteAddr(); a != nil {
			remote = a.String()
		}
		conn.CloseWithError(err)
	}
	slog.Warn("fibhub: dropped a message", "remote", remote, "err", err)
	msg.SetMethodLen(0)
	return msg
}
