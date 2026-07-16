//go:build linux

package clipboard

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"mime"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The Linux clipboard is driven by shelling out to a CLI bridge, since there is
// no cgo-free Go binding for either the Wayland (wlr-data-control) or X11
// selection protocols. Two backends are supported and auto-detected at startup:
// wl-clipboard for Wayland sessions and xclip for X11. Both speak a canonical
// type vocabulary so the watch/read/set logic is shared across them.
//
// Canonical MIME tokens used across backends:
const (
	typeURIList = "text/uri-list"
	typePNG     = "image/png"
	typeText    = "text/plain"
)

// pollInterval is how often Watch checks clipboard state. Both tools offer a
// --watch-style mode, but polling the offered types is simpler and plenty
// responsive for a clipboard manager, and works identically across backends.
const pollInterval = 300 * time.Millisecond

// backend abstracts the underlying clipboard CLI (wl-clipboard or xclip). It
// speaks the canonical tokens above: listTypes reports which of them the
// clipboard currently offers, readType fetches one, and copy writes one.
type backend interface {
	name() string
	listTypes(ctx context.Context) []string
	readType(ctx context.Context, canonical string) ([]byte, error)
	copy(ctx context.Context, canonical string, data []byte) error
}

// watchableBackend is an optional capability: a backend that can notify on
// clipboard changes via a single long-lived process, instead of being polled.
// Backends that implement this avoid spawning a reader subprocess on a timer,
// which on Wayland churns the compositor and flickers the whole screen.
type watchableBackend interface {
	// watchChanges blocks until ctx is cancelled, calling notify once per
	// clipboard change. It returns an error if the watch cannot be sustained.
	watchChanges(ctx context.Context, notify func()) error
}

// shellClipboard implements Clipboard on top of any backend.
type shellClipboard struct {
	b backend
}

// newClipboard auto-detects the session type and available CLI, preferring the
// backend that matches the active display server and falling back to whichever
// tool is actually installed.
func newClipboard() (Clipboard, error) {
	_, hasWayland := os.LookupEnv("WAYLAND_DISPLAY")
	_, hasX11 := os.LookupEnv("DISPLAY")
	switch selectBackend(hasWayland, hasX11, haveWayland(), haveXclip()) {
	case "wayland":
		return &shellClipboard{b: &waylandBackend{}}, nil
	case "x11":
		return &shellClipboard{b: &x11Backend{}}, nil
	default:
		return nil, fmt.Errorf("clipboard: no supported backend found; install wl-clipboard (Wayland) or xclip (X11)")
	}
}

// selectBackend chooses a backend from the session env and installed tooling.
// It prefers the backend matching the active display server, then falls back to
// whichever tool is actually present. Returns "" when nothing usable is found.
func selectBackend(hasWayland, hasX11, wlPresent, xPresent bool) string {
	switch {
	case hasWayland && wlPresent:
		return "wayland"
	case hasX11 && xPresent:
		return "x11"
	case wlPresent:
		return "wayland"
	case xPresent:
		return "x11"
	default:
		return ""
	}
}

func haveWayland() bool {
	_, e1 := exec.LookPath("wl-paste")
	_, e2 := exec.LookPath("wl-copy")
	return e1 == nil && e2 == nil
}

func haveXclip() bool {
	_, err := exec.LookPath("xclip")
	return err == nil
}

// current inspects the offered types richest-first (file > image > text) and
// returns the resulting Item. It returns ok=false when the clipboard is empty
// or holds nothing we can represent.
func (c *shellClipboard) current(ctx context.Context) (item Item, ok bool) {
	types := c.b.listTypes(ctx)
	if len(types) == 0 {
		return Item{}, false
	}

	has := func(t string) bool {
		for _, x := range types {
			if x == t {
				return true
			}
		}
		return false
	}

	switch {
	case has(typeURIList):
		data, err := c.b.readType(ctx, typeURIList)
		if err == nil {
			if item, ok := fileItemFromURIList(data); ok {
				return item, true
			}
		}
		// Fall through to other types if the URI list didn't yield a usable
		// local file (e.g. remote URI or unreadable path).
		fallthrough

	case has(typePNG):
		if has(typePNG) {
			data, err := c.b.readType(ctx, typePNG)
			if err == nil && len(data) > 0 {
				return Item{Kind: KindImage, Mime: typePNG, Data: data}, true
			}
		}
		fallthrough

	case has(typeText):
		if has(typeText) {
			data, err := c.b.readType(ctx, typeText)
			if err == nil && len(data) > 0 {
				return Item{Kind: KindText, Mime: typeText, Data: data}, true
			}
		}
	}

	return Item{}, false
}

// fileItemFromURIList parses a text/uri-list payload, taking the first local
// file:// entry, reading its bytes off disk. Remote or unreadable entries are
// skipped (ok=false) so the caller can fall back to a lesser-rich type.
func fileItemFromURIList(data []byte) (Item, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		u, err := url.Parse(line)
		if err != nil || u.Scheme != "file" {
			continue
		}
		path := u.Path
		if path == "" {
			continue
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		name := filepath.Base(path)
		mimeType := mime.TypeByExtension(filepath.Ext(name))
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		return Item{Kind: KindFile, Mime: mimeType, Name: name, Data: contents}, true
	}
	return Item{}, false
}

// Read returns the current richest clipboard content, best-effort.
func (c *shellClipboard) Read() (Item, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	item, ok := c.current(ctx)
	if !ok {
		return Item{}, nil
	}
	return item, nil
}

// Watch emits an Item each time the clipboard content's hash differs from the
// last seen value. Empty clipboards and duplicate reads are skipped. When the
// backend supports change events (watchableBackend) it is driven by those,
// reading the clipboard only when something actually changed; otherwise it
// falls back to polling. The event path is essential on Wayland: polling by
// respawning wl-paste flickers the whole screen (see watchableBackend).
//
// wl-paste --watch requires the compositor to implement wlr-data-control, an
// extension GNOME/Mutter does not support; there it fails immediately. Rather
// than fall back to polling the same wl-paste backend (which also flickers,
// since wl-paste has no focus-free read path on such compositors), switch to
// xclip if it's installed: xclip talks to the clipboard via XWayland and does
// not trigger the same disruption. This never permanently gives up on syncing.
func (c *shellClipboard) Watch(ctx context.Context, out chan<- Item) error {
	wb, ok := c.b.(watchableBackend)
	if !ok {
		return c.watchPoll(ctx, out)
	}

	err := c.watchEvents(ctx, wb, out)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		if _, alreadyX11 := c.b.(*x11Backend); !alreadyX11 && haveXclip() {
			log.Printf("clipboard: %v; switching to xclip polling", err)
			c.b = &x11Backend{}
		} else {
			log.Printf("clipboard: %v; falling back to polling", err)
		}
		return c.watchPoll(ctx, out)
	}
	return nil
}

// emitter reads the current clipboard content and sends it on out, skipping
// empty clipboards and content identical to the last emit.
type emitter struct {
	lastHash [32]byte
	haveLast bool
}

func (e *emitter) emit(ctx context.Context, c *shellClipboard, out chan<- Item) error {
	item, ok := c.current(ctx)
	if !ok {
		return nil
	}
	hash := sha256.Sum256(item.Data)
	if e.haveLast && hash == e.lastHash {
		return nil
	}
	e.lastHash = hash
	e.haveLast = true
	select {
	case out <- item:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// watchEvents drives emission off the backend's change notifications. It reads
// the clipboard only when notified, so an idle clipboard spawns no readers.
func (c *shellClipboard) watchEvents(ctx context.Context, wb watchableBackend, out chan<- Item) error {
	changes := make(chan struct{}, 1)
	errc := make(chan error, 1)
	go func() {
		errc <- wb.watchChanges(ctx, func() {
			select {
			case changes <- struct{}{}:
			default: // a pending notification already coalesces this one
			}
		})
	}()

	var e emitter
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errc:
			return err
		case <-changes:
			if err := e.emit(ctx, c, out); err != nil {
				return err
			}
		}
	}
}

// watchPoll is the fallback for backends without change events (e.g. xclip).
func (c *shellClipboard) watchPoll(ctx context.Context, out chan<- Item) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var e emitter
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := e.emit(ctx, c, out); err != nil {
				return err
			}
		}
	}
}

// Set writes text or image content to the system clipboard. Files are rejected:
// the daemon persists them to disk rather than the clipboard, so there's
// nothing meaningful to hold.
func (c *shellClipboard) Set(item Item) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	switch item.Kind {
	case KindText:
		return c.b.copy(ctx, typeText, item.Data)
	case KindImage:
		return c.b.copy(ctx, typePNG, item.Data)
	case KindFile:
		return fmt.Errorf("clipboard: cannot Set a file to the clipboard (the daemon puts its saved path on the clipboard as plain text instead)")
	default:
		return fmt.Errorf("clipboard: unknown item kind %q", item.Kind)
	}
}

// --- Wayland backend (wl-clipboard) ---

type waylandBackend struct{}

func (waylandBackend) name() string { return "wayland" }

// waylandType maps a canonical token to the concrete wl-clipboard type atom.
func waylandType(canonical string) string {
	if canonical == typeText {
		return "text/plain;charset=utf-8"
	}
	return canonical
}

func (waylandBackend) listTypes(ctx context.Context) []string {
	// An empty clipboard makes wl-paste exit non-zero; treat that as "no types".
	out, err := exec.CommandContext(ctx, "wl-paste", "--list-types").Output()
	if err != nil {
		return nil
	}
	var offered []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		offered = append(offered, strings.TrimSpace(l))
	}
	return canonicalize(offered)
}

func (waylandBackend) readType(ctx context.Context, canonical string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "wl-paste", "--no-newline", "--type", waylandType(canonical))
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	if err := cmd.Run(); err != nil {
		// text/plain;charset=utf-8 may not be offered even when text/plain is;
		// retry with the bare token before giving up.
		if canonical == typeText {
			cmd = exec.CommandContext(ctx, "wl-paste", "--no-newline", "--type", typeText)
			out.Reset()
			errBuf.Reset()
			cmd.Stdout, cmd.Stderr = &out, &errBuf
			if err2 := cmd.Run(); err2 == nil {
				return out.Bytes(), nil
			}
		}
		return nil, fmt.Errorf("wl-paste --type %s: %w: %s", canonical, err, errBuf.String())
	}
	return out.Bytes(), nil
}

// watchChanges runs a single long-lived `wl-paste --watch` process that invokes
// its command (a no-op `echo`, which prints a newline and ignores the clipboard
// content piped to it) once per clipboard change. Each line read is one change.
// This registers exactly one Wayland client for the daemon's lifetime, unlike
// polling which respawns wl-paste continuously and flickers the compositor.
func (waylandBackend) watchChanges(ctx context.Context, notify func()) error {
	cmd := exec.CommandContext(ctx, "wl-paste", "--watch", "echo")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("wl-paste --watch: %w", err)
	}

	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		notify()
	}
	// Scanner stopped: either ctx cancelled (process killed) or wl-paste died.
	werr := cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if werr != nil {
		return fmt.Errorf("wl-paste --watch exited: %w", werr)
	}
	return fmt.Errorf("wl-paste --watch ended unexpectedly")
}

func (waylandBackend) copy(ctx context.Context, canonical string, data []byte) error {
	cmd := exec.CommandContext(ctx, "wl-copy", "--type", waylandType(canonical))
	cmd.Stdin = bytes.NewReader(data)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("wl-copy --type %s: %w: %s", canonical, err, errBuf.String())
	}
	return nil
}

// --- X11 backend (xclip) ---

type x11Backend struct{}

func (x11Backend) name() string { return "x11" }

func (x11Backend) listTypes(ctx context.Context) []string {
	// An empty selection makes xclip exit non-zero; treat that as "no types".
	out, err := exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-t", "TARGETS", "-o").Output()
	if err != nil {
		return nil
	}
	var offered []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		offered = append(offered, strings.TrimSpace(l))
	}
	return canonicalize(offered)
}

func (x11Backend) readType(ctx context.Context, canonical string) ([]byte, error) {
	// For text, prefer the UTF8_STRING atom which is what most apps offer.
	target := canonical
	if canonical == typeText {
		target = "UTF8_STRING"
	}
	data, err := x11read(ctx, target)
	if err != nil && canonical == typeText {
		return x11read(ctx, typeText) // fall back to the bare MIME atom
	}
	return data, err
}

func x11read(ctx context.Context, target string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-t", target, "-o")
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("xclip -t %s -o: %w: %s", target, err, errBuf.String())
	}
	return out.Bytes(), nil
}

func (x11Backend) copy(ctx context.Context, canonical string, data []byte) error {
	// xclip reads all of stdin, then forks: the invoked process exits and a
	// detached grandchild stays resident to serve the selection. Do not tie
	// this to ctx: cancelling the timeout would kill the resident process and
	// drop the selection. But we DO need to wait (Run, not Start) for the initial
	// process to exit — Start returns as soon as the process is launched, before
	// the stdin data has actually finished being written, so callers could see
	// "success" while xclip is still empty-handed and not yet holding the data.
	cmd := exec.Command("xclip", "-selection", "clipboard", "-t", canonical, "-i")
	cmd.Stdin = bytes.NewReader(data)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("xclip -t %s -i: %w: %s", canonical, err, bytes.TrimSpace(errBuf.Bytes()))
	}
	return nil
}

// canonicalize reduces a backend's raw offered type atoms to the canonical set
// this package reasons about, de-duplicating and dropping anything unsupported.
func canonicalize(raw []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(t string) {
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, t := range raw {
		switch t {
		case typeURIList:
			add(typeURIList)
		case typePNG:
			add(typePNG)
		case typeText, "text/plain;charset=utf-8", "UTF8_STRING", "STRING", "TEXT":
			add(typeText)
		}
	}
	return out
}
