// Package tailnet enumerates machines on the Tailscale network and resolves
// target names to addresses.
package tailnet

import (
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// Peer is a machine on the tailnet.
type Peer struct {
	Name   string
	IP     string
	Online bool
}

// tsNode mirrors the fields we care about from a Tailscale status node (self
// or peer). Parsed defensively: fields absent from the JSON just zero-value.
type tsNode struct {
	HostName     string   `json:"HostName"`
	DNSName      string   `json:"DNSName"`
	TailscaleIPs []string `json:"TailscaleIPs"`
	Online       bool     `json:"Online"`
}

// tsStatus mirrors the subset of `tailscale status --json` we need.
type tsStatus struct {
	Self *tsNode
	Peer map[string]*tsNode
}

// status runs `tailscale status --json` and parses it.
func status() (*tsStatus, error) {
	out, err := exec.Command("tailscale", "status", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("tailnet: run tailscale status: %w", err)
	}
	var st tsStatus
	if err := json.Unmarshal(out, &st); err != nil {
		return nil, fmt.Errorf("tailnet: parse tailscale status: %w", err)
	}
	return &st, nil
}

// firstIPv4 returns the first IPv4 address (100.x tailnet address) in ips,
// or "" if none is found.
func firstIPv4(ips []string) string {
	for _, ip := range ips {
		if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() != nil {
			return ip
		}
	}
	return ""
}

// displayName picks a human-readable name for a node: HostName if set,
// otherwise the first label of DNSName with any trailing dot trimmed.
func displayName(n *tsNode) string {
	if n.HostName != "" {
		return n.HostName
	}
	dns := strings.TrimSuffix(n.DNSName, ".")
	if i := strings.IndexByte(dns, '.'); i >= 0 {
		return dns[:i]
	}
	return dns
}

// Peers returns the other machines on the tailnet (self excluded), parsed from
// `tailscale status --json`.
func Peers() ([]Peer, error) {
	st, err := status()
	if err != nil {
		return nil, err
	}
	peers := make([]Peer, 0, len(st.Peer))
	for _, n := range st.Peer {
		if n == nil {
			continue
		}
		ip := firstIPv4(n.TailscaleIPs)
		if ip == "" {
			continue // no tailnet IPv4 (e.g. funnel-ingress infra) -> never a target
		}
		peers = append(peers, Peer{
			Name:   displayName(n),
			IP:     ip,
			Online: n.Online,
		})
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].Name < peers[j].Name })
	return peers, nil
}

// SelfIP returns this machine's primary tailnet IP (100.x.y.z).
func SelfIP() (string, error) {
	st, err := status()
	if err != nil {
		return "", err
	}
	if st.Self == nil {
		return "", fmt.Errorf("tailnet: no self node in tailscale status")
	}
	ip := firstIPv4(st.Self.TailscaleIPs)
	if ip == "" {
		return "", fmt.Errorf("tailnet: no IPv4 tailnet address found for self")
	}
	return ip, nil
}

// Resolve maps a target name (or IP) to a host:port address for the given port.
func Resolve(name string, port int) (string, error) {
	if parsed := net.ParseIP(name); parsed != nil {
		return net.JoinHostPort(name, strconv.Itoa(port)), nil
	}

	st, err := status()
	if err != nil {
		return "", err
	}

	lname := strings.ToLower(name)
	for _, n := range st.Peer {
		if n == nil {
			continue
		}
		dns := strings.ToLower(strings.TrimSuffix(n.DNSName, "."))
		dnsFirstLabel := dns
		if i := strings.IndexByte(dns, '.'); i >= 0 {
			dnsFirstLabel = dns[:i]
		}
		if strings.EqualFold(n.HostName, name) || dns == lname || dnsFirstLabel == lname {
			ip := firstIPv4(n.TailscaleIPs)
			if ip == "" {
				return "", fmt.Errorf("tailnet: peer %q has no IPv4 tailnet address", name)
			}
			return net.JoinHostPort(ip, strconv.Itoa(port)), nil
		}
	}

	return "", fmt.Errorf("tailnet: no peer found matching %q", name)
}

// ListenAddr returns the host:port the daemon binds to and the CLI connects to:
// the tailnet IP when available (FR-015), else 127.0.0.1 (never 0.0.0.0). Both
// the server and CLI call this so they always agree.
func ListenAddr(port int) string {
	if ip, err := SelfIP(); err == nil {
		return net.JoinHostPort(ip, strconv.Itoa(port))
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}
