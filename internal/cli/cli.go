// Package cli implements the client subcommands (peers, target, on, off,
// status) as a thin HTTP client to the local daemon.
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"clippy/internal/config"
	"clippy/internal/tailnet"
	"clippy/internal/transport"
)

// httpTimeout bounds every request to the local daemon.
const httpTimeout = 5 * time.Second

// Run dispatches a client subcommand. args[0] is the subcommand name.
func Run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("cli: no subcommand given")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("cli: load config: %w", err)
	}
	c := &client{
		base:  "http://" + tailnet.ListenAddr(cfg.Port),
		token: cfg.Token,
		hc:    &http.Client{Timeout: httpTimeout},
	}

	switch args[0] {
	case "peers":
		return c.peers()
	case "target":
		if len(args) < 2 || args[1] == "" {
			return fmt.Errorf("cli: target requires a name, e.g. `clippy target mymac`")
		}
		return c.target(args[1])
	case "on":
		return c.sync(true)
	case "off":
		return c.sync(false)
	case "status":
		return c.status()
	default:
		return fmt.Errorf("cli: unknown subcommand %q", args[0])
	}
}

// client is a thin HTTP wrapper for talking to the local daemon.
type client struct {
	base  string
	token string
	hc    *http.Client
}

// do sends a request to path and decodes a 200 JSON response into out (if
// out is non-nil). It translates connection and auth failures into
// friendly errors.
func (c *client) do(method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("cli: encode request: %w", err)
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return fmt.Errorf("cli: build request: %w", err)
	}
	req.Header.Set(transport.HdrAuth, transport.BearerPfx+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		fmt.Println("is the daemon running? (clippy serve)")
		return fmt.Errorf("cli: connect to daemon: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("cli: unauthorized — token mismatch")
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("cli: daemon returned %s: %s", resp.Status, bytes.TrimSpace(msg))
	}

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("cli: decode response: %w", err)
		}
	}
	return nil
}

// peers prints the tailnet peers as a table.
func (c *client) peers() error {
	var peers []transport.PeerDTO
	if err := c.do(http.MethodGet, transport.PathPeers, nil, &peers); err != nil {
		return err
	}
	if len(peers) == 0 {
		fmt.Println("(none)")
		return nil
	}
	fmt.Printf("%-20s %-16s %s\n", "NAME", "IP", "ONLINE")
	for _, p := range peers {
		dot := "○"
		if p.Online {
			dot = "●"
		}
		fmt.Printf("%-20s %-16s %s\n", p.Name, p.IP, dot)
	}
	return nil
}

// target sets the active target peer.
func (c *client) target(name string) error {
	req := transport.TargetReq{Target: name}
	if err := c.do(http.MethodPost, transport.PathTarget, req, nil); err != nil {
		return err
	}
	fmt.Printf("target set to %q\n", name)
	return nil
}

// sync enables or disables sending.
func (c *client) sync(enabled bool) error {
	req := transport.SyncReq{Enabled: enabled}
	if err := c.do(http.MethodPost, transport.PathSync, req, nil); err != nil {
		return err
	}
	if enabled {
		fmt.Println("sync on")
	} else {
		fmt.Println("sync off")
	}
	return nil
}

// status prints the daemon's current state and how to reach it.
func (c *client) status() error {
	var st transport.StatusDTO
	if err := c.do(http.MethodGet, transport.PathStatus, nil, &st); err != nil {
		return err
	}
	target := st.Target
	if target == "" {
		target = "(none)"
	}
	syncState := "off"
	if st.SyncEnabled {
		syncState = "on"
	}
	fmt.Printf("version:    %s\n", st.Version)
	fmt.Printf("sync:       %s\n", syncState)
	fmt.Printf("target:     %s\n", target)
	fmt.Printf("tailscale:  %s\n", st.TailscaleIP)
	fmt.Printf("daemon:     %s\n", c.base)
	return nil
}
