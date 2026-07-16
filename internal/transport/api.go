// Package transport defines the HTTP contract between machines (and phones) and
// the daemon, plus the client and server that speak it.
//
// This file (api.go) is the shared truth: routes, headers, DTOs, and the
// Handler interface the daemon implements. The server dispatches HTTP requests
// to a Handler; the client POSTs payloads to a peer's /v1/clip.
package transport

import "clippy/internal/payload"

// Route paths.
const (
	PathClip   = "/v1/clip"   // POST: receive content (peer/phone -> daemon)
	PathStatus = "/v1/status" // GET:  daemon status
	PathTarget = "/v1/target" // POST: set active target
	PathSync   = "/v1/sync"   // POST: enable/disable sending
	PathPeers  = "/v1/peers"  // GET:  list tailnet peers
	PathDocker = "/v1/docker" // GET: list containers; POST: select one
	PathSend   = "/v1/send"   // POST: multipart relay from webapp/phone
	PathRoot   = "/"          // GET:  webapp UI + static assets
)

// Header names for payload metadata.
const (
	HdrKind = "X-Clippy-Kind"
	HdrName = "X-Clippy-Name"
	HdrMime = "X-Clippy-Mime"
	HdrSha  = "X-Clippy-Sha256"
)

// StatusDTO is returned by GET /v1/status.
type StatusDTO struct {
	Version     string `json:"version"`
	SyncEnabled bool   `json:"sync_enabled"`
	Target      string `json:"target"`
	Docker      string `json:"docker_container"`
	TailscaleIP string `json:"tailscale_ip"`
}

type ContainerDTO struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// PeerDTO is one entry returned by GET /v1/peers.
type PeerDTO struct {
	Name   string `json:"name"`
	IP     string `json:"ip"`
	Online bool   `json:"online"`
}

// TargetReq is the body of POST /v1/target.
type TargetReq struct {
	Target string `json:"target"`
}

// SyncReq is the body of POST /v1/sync.
type SyncReq struct {
	Enabled bool `json:"enabled"`
}

type DockerReq struct {
	Container string `json:"container"`
}

// Handler is implemented by the daemon; the HTTP server dispatches to it.
type Handler interface {
	// Receive handles inbound content: sets the clipboard (text/image) or saves
	// a file. Returns the HTTP status to send (200/400/413/500).
	Receive(kind, name, mime, sha string, body []byte) (int, error)
	// Status reports current daemon state.
	Status() StatusDTO
	// SetTarget changes the active target and persists it.
	SetTarget(target string) error
	// SetSync toggles sending and persists it.
	SetSync(enabled bool) error
	SetDocker(container string) error
	Containers() ([]ContainerDTO, error)
	// Peers lists the machines on the tailnet.
	Peers() ([]PeerDTO, error)
	// Relay forwards a payload (built from a webapp/phone upload) to target.
	Relay(target string, p *payload.Payload) error
}
