// Package payload defines the unit of clipboard content moving between machines.
package payload

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// MaxSize is the per-item cap (FR-004). Items larger than this are never sent.
const MaxSize = 20 << 20 // 20 MiB

// Kind classifies a payload's content.
type Kind string

const (
	KindText  Kind = "text"
	KindImage Kind = "image"
	KindFile  Kind = "file"
)

// Payload is a unit of clipboard content in transit.
type Payload struct {
	ID     string // first 16 hex chars of Sha256, for logging
	Kind   Kind
	Mime   string
	Name   string // original filename, for KindFile
	Sha256 string
	Size   int
	Data   []byte
}

// New builds a Payload from raw bytes, computing its hash and size.
func New(kind Kind, mime, name string, data []byte) *Payload {
	h := Hash(data)
	return &Payload{
		ID:     h[:16],
		Kind:   kind,
		Mime:   mime,
		Name:   name,
		Sha256: h,
		Size:   len(data),
		Data:   data,
	}
}

// OverCap reports whether the payload exceeds MaxSize.
func (p *Payload) OverCap() bool { return p.Size > MaxSize }

func (p *Payload) String() string {
	return fmt.Sprintf("%s/%s %q %dB", p.Kind, p.Mime, p.Name, p.Size)
}

// Hash returns the hex-encoded sha256 of data.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
