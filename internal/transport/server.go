package transport

import (
	"context"
	"errors"
	"net/http"

	"clippy/internal/config"
)

// Server exposes the daemon over HTTP: the /v1 API for peers, the control
// endpoints for the local CLI, and the webapp UI. It binds to the tailnet
// interface (FR-015) and authenticates with the shared token (FR-009).
type Server struct {
	cfg *config.Config
	h   Handler
	web http.Handler
}

// NewServer builds a Server. web serves the static webapp UI at "/".
func NewServer(cfg *config.Config, h Handler, web http.Handler) *Server {
	return &Server{cfg: cfg, h: h, web: web}
}

// Run binds to the tailnet IP on cfg.Port and serves until ctx is cancelled.
//
// TODO(agent D): implement — auth middleware, route table dispatching to s.h,
// multipart /v1/send relay, mount s.web at "/", graceful shutdown on ctx.
func (s *Server) Run(ctx context.Context) error {
	_ = ctx
	return errors.New("transport: server not implemented")
}
