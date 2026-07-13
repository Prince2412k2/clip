// Package tailnet enumerates machines on the Tailscale network and resolves
// target names to addresses.
package tailnet

import (
	"errors"
	"net"
	"strconv"
)

// errNotImpl is returned by the Wave-0 stubs until agent C implements them.
var errNotImpl = errors.New("tailnet: not implemented")

// Peer is a machine on the tailnet.
type Peer struct {
	Name   string
	IP     string
	Online bool
}

// Peers returns the other machines on the tailnet (self excluded), parsed from
// `tailscale status --json`.
func Peers() ([]Peer, error) {
	return nil, errNotImpl // TODO(agent C)
}

// SelfIP returns this machine's primary tailnet IP (100.x.y.z).
func SelfIP() (string, error) {
	return "", errNotImpl // TODO(agent C)
}

// Resolve maps a target name (or IP) to a host:port address for the given port.
func Resolve(name string, port int) (string, error) {
	return "", errNotImpl // TODO(agent C)
}

// ListenAddr returns the host:port the daemon binds to and the CLI connects to:
// the tailnet IP when available (FR-015), else 127.0.0.1 (never 0.0.0.0). Both
// the server and CLI call this so they always agree.
func ListenAddr(port int) string {
	// TODO(agent C): prefer SelfIP() when available; this fallback keeps local
	// runs working without Tailscale.
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}
