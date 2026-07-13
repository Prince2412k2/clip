//go:build linux

package clipboard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"mime"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// wlClipboard shells out to the wl-clipboard tools (wl-paste/wl-copy) to
// implement the Clipboard interface for Wayland sessions. There is no
// official Go binding for wlr-data-control, and wl-clipboard is the standard
// CLI bridge, so we avoid cgo entirely by driving it as subprocesses.
type wlClipboard struct{}

// pollInterval is how often Watch checks clipboard state. wl-paste --watch
// exists but pipes selection contents through a spawned command per change
// with no clean way to also query MIME types for that same event, so polling
// --list-types is simpler and plenty responsive for a clipboard manager.
const pollInterval = 300 * time.Millisecond

func newClipboard() (Clipboard, error) {
	if _, err := exec.LookPath("wl-paste"); err != nil {
		return nil, fmt.Errorf("clipboard: wl-paste not found (install wl-clipboard): %w", err)
	}
	if _, err := exec.LookPath("wl-copy"); err != nil {
		return nil, fmt.Errorf("clipboard: wl-copy not found (install wl-clipboard): %w", err)
	}
	return &wlClipboard{}, nil
}

// listTypes returns the MIME types currently offered on the clipboard. An
// empty clipboard makes wl-paste exit non-zero; treat that as "no types"
// rather than an error.
func (w *wlClipboard) listTypes(ctx context.Context) []string {
	cmd := exec.CommandContext(ctx, "wl-paste", "--list-types")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	types := make([]string, 0, len(lines))
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			types = append(types, l)
		}
	}
	return types
}

// readType fetches the clipboard content for a specific MIME type.
func (w *wlClipboard) readType(ctx context.Context, mimeType string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "wl-paste", "--no-newline", "--type", mimeType)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("wl-paste --type %s: %w: %s", mimeType, err, errBuf.String())
	}
	return out.Bytes(), nil
}

// current inspects the offered MIME types richest-first (file > image > text)
// and returns the resulting Item. It returns ok=false when the clipboard is
// empty or holds nothing we can represent.
func (w *wlClipboard) current(ctx context.Context) (item Item, ok bool) {
	types := w.listTypes(ctx)
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
	case has("text/uri-list"):
		data, err := w.readType(ctx, "text/uri-list")
		if err != nil {
			return Item{}, false
		}
		item, ok := fileItemFromURIList(data)
		if ok {
			return item, true
		}
		// Fall through to other types if the URI list didn't yield a usable
		// local file (e.g. remote URI or unreadable path).

	case has("image/png"):
		data, err := w.readType(ctx, "image/png")
		if err != nil || len(data) == 0 {
			return Item{}, false
		}
		return Item{Kind: KindImage, Mime: "image/png", Data: data}, true

	case has("text/plain;charset=utf-8"):
		data, err := w.readType(ctx, "text/plain;charset=utf-8")
		if err != nil || len(data) == 0 {
			return Item{}, false
		}
		return Item{Kind: KindText, Mime: "text/plain", Data: data}, true

	case has("text/plain"):
		data, err := w.readType(ctx, "text/plain")
		if err != nil || len(data) == 0 {
			return Item{}, false
		}
		return Item{Kind: KindText, Mime: "text/plain", Data: data}, true
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
func (w *wlClipboard) Read() (Item, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	item, ok := w.current(ctx)
	if !ok {
		return Item{}, nil
	}
	return item, nil
}

// Watch polls the clipboard for changes and emits an Item each time the
// content's hash differs from the last seen value. Empty clipboards and
// duplicate reads are skipped.
func (w *wlClipboard) Watch(ctx context.Context, out chan<- Item) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var lastHash [32]byte
	haveLast := false

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			item, ok := w.current(ctx)
			if !ok {
				continue
			}
			hash := sha256.Sum256(item.Data)
			if haveLast && hash == lastHash {
				continue
			}
			lastHash = hash
			haveLast = true

			select {
			case out <- item:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// Set writes text or image content to the system clipboard via wl-copy.
// Files are rejected: the daemon persists them to disk rather than the
// clipboard, so there's nothing meaningful for wl-copy to hold.
func (w *wlClipboard) Set(item Item) error {
	switch item.Kind {
	case KindText:
		return w.copy("text/plain", item.Data)
	case KindImage:
		return w.copy("image/png", item.Data)
	case KindFile:
		return fmt.Errorf("clipboard: cannot Set a file to the clipboard (files are saved to disk, not copied)")
	default:
		return fmt.Errorf("clipboard: unknown item kind %q", item.Kind)
	}
}

func (w *wlClipboard) copy(mimeType string, data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "wl-copy", "--type", mimeType)
	cmd.Stdin = bytes.NewReader(data)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("wl-copy --type %s: %w: %s", mimeType, err, errBuf.String())
	}
	return nil
}
