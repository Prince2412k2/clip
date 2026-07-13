package transport

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"mime"
	"net/http"
	"strings"
	"time"

	"clippy/internal/config"
	"clippy/internal/payload"
	"clippy/internal/tailnet"
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
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()

	mux.Handle(PathClip, s.auth(http.HandlerFunc(s.handleClip)))
	mux.Handle(PathStatus, s.auth(http.HandlerFunc(s.handleStatus)))
	mux.Handle(PathTarget, s.auth(http.HandlerFunc(s.handleTarget)))
	mux.Handle(PathSync, s.auth(http.HandlerFunc(s.handleSync)))
	mux.Handle(PathPeers, s.auth(http.HandlerFunc(s.handlePeers)))
	mux.Handle(PathSend, s.auth(http.HandlerFunc(s.handleSend)))
	mux.Handle(PathRoot, s.web)

	addr := tailnet.ListenAddr(s.cfg.Port)
	srv := &http.Server{
		Addr:    addr,
		Handler: logRequests(mux),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return nil
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	}
}

// logRequests logs method, path, and status for every request.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s %d", r.Method, r.URL.Path, sw.status)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(status int) {
	sw.status = status
	sw.ResponseWriter.WriteHeader(status)
}

// auth wraps h with bearer-token authentication (FR-009).
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Get(HdrAuth)
		want := BearerPfx + s.cfg.Token
		if !strings.HasPrefix(hdr, BearerPfx) || subtle.ConstantTimeCompare([]byte(hdr), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleClip(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	kind := r.Header.Get(HdrKind)
	name := r.Header.Get(HdrName)
	mimeType := r.Header.Get(HdrMime)
	sha := r.Header.Get(HdrSha)

	body, err := io.ReadAll(io.LimitReader(r.Body, payload.MaxSize+1))
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	if len(body) > payload.MaxSize {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}

	status, err := s.h.Receive(kind, name, mimeType, sha, body)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	w.WriteHeader(status)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.h.Status())
}

func (s *Server) handleTarget(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req TargetReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.h.SetTarget(req.Target); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req SyncReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.h.SetSync(req.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	peers, err := s.h.Peers()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if peers == nil {
		peers = []PeerDTO{}
	}
	writeJSON(w, http.StatusOK, peers)
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseMultipartForm(8 << 20); err != nil {
		http.Error(w, "bad multipart form", http.StatusBadRequest)
		return
	}

	target := r.FormValue("target")
	if target == "" {
		http.Error(w, "target is required", http.StatusBadRequest)
		return
	}

	var p *payload.Payload

	if text := r.FormValue("text"); text != "" {
		p = payload.New(payload.KindText, "text/plain", "", []byte(text))
	} else {
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "text or file is required", http.StatusBadRequest)
			return
		}
		defer file.Close()

		data, err := io.ReadAll(io.LimitReader(file, payload.MaxSize+1))
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		if len(data) > payload.MaxSize {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}

		ct := header.Header.Get("Content-Type")
		if ct == "" {
			ct = "application/octet-stream"
		}
		// Strip any parameters (e.g. "; charset=...") from the content type.
		if mt, _, err := mime.ParseMediaType(ct); err == nil {
			ct = mt
		}

		if strings.HasPrefix(ct, "image/") {
			p = payload.New(payload.KindImage, ct, header.Filename, data)
		} else {
			p = payload.New(payload.KindFile, ct, header.Filename, data)
		}
	}

	if err := s.h.Relay(target, p); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json response: %v", err)
	}
}
