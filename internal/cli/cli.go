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
		base: "http://" + tailnet.ListenAddr(cfg.Port),
		hc:   &http.Client{Timeout: httpTimeout},
	}

	switch args[0] {
	case "peers":
		return c.peers()
	case "target":
		if len(args) < 2 || args[1] == "" {
			return c.pickTargetInteractive()
		}
		return c.target(args[1])
	case "docker":
		if len(args) < 2 || args[1] == "" {
			return c.pickDockerInteractive()
		}
		if args[1] == "off" {
			return c.docker("")
		}
		return c.docker(args[1])
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
	base string
	hc   *http.Client
}

// do sends a request to path and decodes a 200 JSON response into out (if
// out is non-nil). It translates connection failures into friendly errors.
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
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		fmt.Println("is the daemon running? (clippy serve)")
		return fmt.Errorf("cli: connect to daemon: %w", err)
	}
	defer resp.Body.Close()

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

// pickTargetInteractive fetches peers, lets the user arrow-key through them
// inline, then sets the chosen one as the active target.
func (c *client) pickTargetInteractive() error {
	var peers []transport.PeerDTO
	if err := c.do(http.MethodGet, transport.PathPeers, nil, &peers); err != nil {
		return err
	}
	if len(peers) == 0 {
		fmt.Println("(no peers found)")
		return nil
	}

	var st transport.StatusDTO
	_ = c.do(http.MethodGet, transport.PathStatus, nil, &st) // current target is a nice-to-have; ignore errors

	name, err := pickTarget(peers, st.Target)
	if err != nil {
		return err
	}
	if name == "" {
		fmt.Println("cancelled")
		return nil
	}
	return c.target(name)
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

func (c *client) pickDockerInteractive() error {
	var containers []transport.ContainerDTO
	if err := c.do(http.MethodGet, transport.PathDocker, nil, &containers); err != nil {
		return err
	}
	if len(containers) == 0 {
		fmt.Println("(no Docker containers found)")
		return nil
	}
	var st transport.StatusDTO
	_ = c.do(http.MethodGet, transport.PathStatus, nil, &st)
	name, err := pickContainer(containers, st.Docker)
	if err != nil {
		return err
	}
	if name == "" {
		fmt.Println("cancelled")
		return nil
	}
	return c.docker(name)
}

func (c *client) docker(name string) error {
	if err := c.do(http.MethodPost, transport.PathDocker, transport.DockerReq{Container: name}, nil); err != nil {
		return err
	}
	if name == "" {
		fmt.Println("Docker integration disabled")
		return nil
	}
	fmt.Printf("Docker container set to %q\n", name)
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
	docker := st.Docker
	if docker == "" {
		docker = "(none)"
	}
	fmt.Printf("docker:     %s\n", docker)
	fmt.Printf("tailscale:  %s\n", st.TailscaleIP)
	fmt.Printf("daemon:     %s\n", c.base)
	return nil
}
