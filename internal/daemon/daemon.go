// Package daemon ties the clipboard watcher, transport, and tailnet together.
// It implements transport.Handler and owns the two correctness invariants:
// dedupe (FR-005) and send/receive echo suppression (FR-006).
package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"clippy/internal/clipboard"
	"clippy/internal/config"
	"clippy/internal/payload"
	"clippy/internal/tailnet"
	"clippy/internal/transport"
)

// Version is reported via /v1/status.
const Version = "0.1.0"

// maxRecentlySeen caps the echo-suppression map so entries that are never
// consumed by a watcher fire (e.g. backend-suppressed identical writes) cannot
// accumulate without bound on a long-running daemon.
const maxRecentlySeen = 64

// sender delivers a payload to a peer. It is a package var so tests can
// substitute a recorder in place of a real network send.
var sender = func(ctx context.Context, addr string, p *payload.Payload) error {
	return transport.NewClient().Send(ctx, addr, p)
}

// Daemon watches the local clipboard and pushes changes to the selected target,
// and applies inbound content received from peers/phones.
type Daemon struct {
	mu   sync.Mutex
	cfg  *config.Config
	clip clipboard.Clipboard

	// recentlySeen holds hashes just written to the clipboard via Receive, so
	// the watcher does not echo them back to the sender (FR-006). Each entry is
	// consumed by the first matching watcher fire.
	recentlySeen map[string]bool
	// lastSent is the hash of the last content pushed, to skip identical
	// repeats and chatty watcher re-fires (FR-005).
	lastSent string
}

// New constructs a Daemon over the given config and clipboard backend.
func New(cfg *config.Config, clip clipboard.Clipboard) *Daemon {
	return &Daemon{cfg: cfg, clip: clip, recentlySeen: map[string]bool{}}
}

// Run starts the clipboard watcher and blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	out := make(chan clipboard.Item, 8)
	go func() {
		if err := d.clip.Watch(ctx, out); err != nil && ctx.Err() == nil {
			log.Printf("clipboard watch error: %v", err)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case it := <-out:
			d.onLocalChange(ctx, it)
		}
	}
}

// onLocalChange decides whether a local clipboard change should be pushed.
func (d *Daemon) onLocalChange(ctx context.Context, it clipboard.Item) {
	if len(it.Data) == 0 {
		return
	}
	h := payload.Hash(it.Data)

	d.mu.Lock()
	if d.recentlySeen[h] {
		delete(d.recentlySeen, h) // our own Set caused this fire; swallow it
		d.mu.Unlock()
		return
	}
	if h == d.lastSent {
		d.mu.Unlock()
		return
	}
	enabled, target, port := d.cfg.SyncEnabled, d.cfg.Target, d.cfg.Port
	d.mu.Unlock()

	if it.Kind == clipboard.KindImage {
		name := it.Name
		if name == "" {
			name = "clippy-image" + extFromMime(it.Mime)
		}
		dest, err := d.saveFile(name, it.Data)
		if err != nil {
			log.Printf("save local image: %v", err)
		} else {
			d.copyIntoDocker(dest)
			d.copyPathToClipboard(dest)
			log.Printf("saved local image -> %s (path copied to clipboard)", dest)
		}
	}

	// Plain text copies on this machine are never auto-sent outward, even
	// with sync on: this fleet only pushes text phone -> PC, not PC -> peers.
	// Files and images still sync out normally.
	if it.Kind == clipboard.KindText {
		return
	}

	if !enabled {
		return
	}
	if target == "" {
		log.Printf("clipboard changed but no target set; skipping")
		return
	}

	p := payload.New(payload.Kind(it.Kind), it.Mime, it.Name, it.Data)
	if p.OverCap() {
		log.Printf("skip %s: %d bytes exceeds %d cap", p.Kind, p.Size, payload.MaxSize)
		return
	}
	addr, err := tailnet.Resolve(target, port)
	if err != nil {
		log.Printf("resolve target %q: %v", target, err)
		return
	}
	if err := sender(ctx, addr, p); err != nil {
		log.Printf("send to %s: %v", target, err)
		return
	}
	d.mu.Lock()
	d.lastSent = h
	d.mu.Unlock()
	log.Printf("sent %s -> %s", p, target)
}

// --- transport.Handler ---

// Receive applies inbound content from a peer or phone.
func (d *Daemon) Receive(kind, name, mime, sha string, body []byte) (int, error) {
	if len(body) > payload.MaxSize {
		return 413, fmt.Errorf("payload %d bytes exceeds cap", len(body))
	}
	switch payload.Kind(kind) {
	case payload.KindFile, payload.KindImage:
		if name == "" && payload.Kind(kind) == payload.KindImage {
			name = "clippy-image" + extFromMime(mime)
		}
		dest, err := d.saveFile(name, body)
		if err != nil {
			return 500, err
		}

		d.copyIntoDocker(dest)

		// The clipboard gets the absolute path as plain text (pasteable
		// anywhere, including terminals/TUIs) rather than the file's raw
		// bytes. Record the hash of THAT path text, not of body, before
		// setting the clipboard: the watcher will see the path string as the
		// new clipboard content and this is what must match to suppress the
		// echo (FR-006).
		if err := d.copyPathToClipboard(dest); err != nil {
			log.Printf("received %s -> %s (clipboard copy failed: %v)", kind, dest, err)
			return 200, nil
		}
		log.Printf("received %s -> %s (path copied to clipboard)", kind, dest)
		return 200, nil
	case payload.KindText:
		// Record the hash BEFORE setting the clipboard so the watcher, which
		// will fire on our own write, suppresses the echo (FR-006).
		d.mu.Lock()
		// Entries only need to survive to the next watcher poll (~300ms); cap the
		// map so unconsumed entries (backend-suppressed writes) can't leak forever.
		if len(d.recentlySeen) >= maxRecentlySeen {
			d.recentlySeen = map[string]bool{}
		}
		d.recentlySeen[payload.Hash(body)] = true
		d.mu.Unlock()
		if err := d.clip.Set(clipboard.Item{
			Kind: clipboard.Kind(kind), Mime: mime, Name: name, Data: body,
		}); err != nil {
			return 500, err
		}
		log.Printf("received %s (%d bytes) -> clipboard", kind, len(body))
		return 200, nil
	default:
		return 400, fmt.Errorf("unknown kind %q", kind)
	}
}

// Status reports current daemon state.
func (d *Daemon) Status() transport.StatusDTO {
	d.mu.Lock()
	defer d.mu.Unlock()
	ip, _ := tailnet.SelfIP()
	return transport.StatusDTO{
		Version:     Version,
		SyncEnabled: d.cfg.SyncEnabled,
		Target:      d.cfg.Target,
		Docker:      d.cfg.DockerContainer,
		TailscaleIP: ip,
	}
}

// SetTarget changes the active target and persists it.
func (d *Daemon) SetTarget(target string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cfg.Target = target
	return d.cfg.Save()
}

// SetSync toggles sending and persists it.
func (d *Daemon) SetSync(enabled bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cfg.SyncEnabled = enabled
	return d.cfg.Save()
}

func (d *Daemon) SetDocker(container string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cfg.DockerContainer = container
	return d.cfg.Save()
}

func (d *Daemon) Containers() ([]transport.ContainerDTO, error) {
	out, err := exec.Command("docker", "ps", "-a", "--format", "{{.Names}}\t{{.Status}}").Output()
	if err != nil {
		return nil, fmt.Errorf("list docker containers: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	containers := make([]transport.ContainerDTO, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		name, status, _ := strings.Cut(line, "\t")
		containers = append(containers, transport.ContainerDTO{Name: name, Status: status})
	}
	return containers, nil
}

// Peers lists the machines on the tailnet.
func (d *Daemon) Peers() ([]transport.PeerDTO, error) {
	ps, err := tailnet.Peers()
	if err != nil {
		return nil, err
	}
	out := make([]transport.PeerDTO, 0, len(ps))
	for _, p := range ps {
		out = append(out, transport.PeerDTO{Name: p.Name, IP: p.IP, Online: p.Online})
	}
	return out, nil
}

// Relay forwards a webapp/phone upload to the chosen target.
func (d *Daemon) Relay(target string, p *payload.Payload) error {
	d.mu.Lock()
	port := d.cfg.Port
	d.mu.Unlock()
	if p.OverCap() {
		return fmt.Errorf("payload %d bytes exceeds cap", p.Size)
	}
	addr, err := tailnet.Resolve(target, port)
	if err != nil {
		return err
	}
	return sender(context.Background(), addr, p)
}

// saveFile writes a received file into the configured receive dir, de-colliding
// the filename so an existing file is never overwritten (FR-008).
func (d *Daemon) saveFile(name string, body []byte) (string, error) {
	d.mu.Lock()
	dir := d.cfg.RecvDir
	d.mu.Unlock()
	if name == "" {
		name = "clippy-file"
	}
	name = filepath.Base(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dest := dedupePath(filepath.Join(dir, name))
	if err := os.WriteFile(dest, body, 0o644); err != nil {
		return "", err
	}
	return dest, nil
}

func (d *Daemon) copyIntoDocker(path string) {
	d.mu.Lock()
	container := d.cfg.DockerContainer
	d.mu.Unlock()
	if container == "" {
		return
	}
	if err := copyToContainer(container, path); err != nil {
		log.Printf("docker cp %s -> %s: %v", path, container, err)
	}
}

func (d *Daemon) copyPathToClipboard(path string) error {
	pathBytes := []byte(path)
	d.mu.Lock()
	if len(d.recentlySeen) >= maxRecentlySeen {
		d.recentlySeen = map[string]bool{}
	}
	d.recentlySeen[payload.Hash(pathBytes)] = true
	d.mu.Unlock()
	return d.clip.Set(clipboard.Item{Kind: clipboard.KindText, Mime: "text/plain", Data: pathBytes})
}

// copyToContainer docker-cp's an already-saved file into the configured
// container at the same absolute path. docker cp does not create missing
// parent directories, so mkdir -p inside the container first. A package var
// so tests can substitute a recorder in place of the real docker CLI.
var copyToContainer = func(container, path string) error {
	dir := filepath.Dir(path)
	if out, err := exec.Command("docker", "exec", container, "mkdir", "-p", dir).CombinedOutput(); err != nil {
		return fmt.Errorf("mkdir -p %s in %s: %w: %s", dir, container, err, bytes.TrimSpace(out))
	}
	if out, err := exec.Command("docker", "cp", path, container+":"+path).CombinedOutput(); err != nil {
		return fmt.Errorf("docker cp %s -> %s:%s: %w: %s", path, container, path, err, bytes.TrimSpace(out))
	}
	return nil
}

// extFromMime returns a best-effort file extension for a MIME type, used when
// a received image has no filename (e.g. synced from a peer's system
// clipboard, which carries no name).
func extFromMime(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ""
	}
}

func dedupePath(p string) string {
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
}
