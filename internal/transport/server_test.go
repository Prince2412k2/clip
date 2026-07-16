package transport

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"clippy/internal/config"
	"clippy/internal/payload"
)

// fakeHandler is a no-op Handler for exercising the HTTP routes.
type fakeHandler struct{}

func (fakeHandler) Receive(kind, name, mime, sha string, body []byte) (int, error) {
	return http.StatusOK, nil
}
func (fakeHandler) Status() StatusDTO                    { return StatusDTO{Version: "test"} }
func (fakeHandler) SetTarget(string) error               { return nil }
func (fakeHandler) SetSync(bool) error                   { return nil }
func (fakeHandler) SetDocker(string) error               { return nil }
func (fakeHandler) Containers() ([]ContainerDTO, error)  { return []ContainerDTO{}, nil }
func (fakeHandler) Peers() ([]PeerDTO, error)            { return []PeerDTO{}, nil }
func (fakeHandler) Relay(string, *payload.Payload) error { return nil }

// TestNoAuthRequired guards the decision to drop bearer-token auth: every
// endpoint must respond without an Authorization header (previously 401).
func TestNoAuthRequired(t *testing.T) {
	s := NewServer(&config.Config{Port: 8787}, fakeHandler{}, http.NotFoundHandler())
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// GET endpoints, no Authorization header.
	for _, path := range []string{PathStatus, PathPeers, PathDocker} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s without auth = %d, want 200", path, resp.StatusCode)
		}
	}

	// A POST endpoint (control), no Authorization header.
	resp, err := http.Post(srv.URL+PathSync, "application/json", strings.NewReader(`{"enabled":true}`))
	if err != nil {
		t.Fatalf("POST %s: %v", PathSync, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST %s without auth = %d, want 200", PathSync, resp.StatusCode)
	}
}
