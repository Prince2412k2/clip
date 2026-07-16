package transport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"clippy/internal/payload"
)

// Client sends payloads to a peer daemon's /v1/clip endpoint.
type Client struct {
	hc *http.Client
}

// NewClient returns a Client for talking to peer daemons over the tailnet.
func NewClient() *Client {
	return &Client{hc: &http.Client{Timeout: 30 * time.Second}}
}

// Send POSTs a payload to a peer at addr (host:port).
func (c *Client) Send(ctx context.Context, addr string, p *payload.Payload) error {
	url := fmt.Sprintf("http://%s%s", addr, PathClip)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(p.Data))
	if err != nil {
		return err
	}
	req.Header.Set(HdrKind, string(p.Kind))
	req.Header.Set(HdrName, p.Name)
	req.Header.Set(HdrMime, p.Mime)
	req.Header.Set(HdrSha, p.Sha256)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("send to %s: status %d: %s", addr, resp.StatusCode, bytes.TrimSpace(body))
	}
	return nil
}
