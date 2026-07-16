// Package clipboard provides a cross-platform system-clipboard backend.
//
// The concrete backend is chosen at build time by the per-OS files
// (clipboard_linux.go, clipboard_darwin.go, clipboard_other.go), each of which
// supplies newClipboard(). Backends shell out to platform tools (wl-clipboard,
// pbcopy/pbpaste/osascript) to keep the binary cgo-free and trivially
// cross-compilable.
package clipboard

import (
	"context"
	"errors"
)

// ErrUnsupported is returned by New on platforms without a backend.
var ErrUnsupported = errors.New("clipboard: unsupported platform")

// Kind classifies a clipboard item.
type Kind string

const (
	KindText  Kind = "text"
	KindImage Kind = "image"
	KindFile  Kind = "file"
)

// Item is a single clipboard entry as seen by a platform backend.
type Item struct {
	Kind Kind
	Mime string
	Name string // original filename, for KindFile
	Data []byte
}

// Clipboard is the platform-specific clipboard backend.
type Clipboard interface {
	// Watch emits an Item on every local clipboard change until ctx is
	// cancelled. Implementations should skip empty content and coalesce rapid
	// duplicate fires where cheap. Blocking; the daemon runs it in a goroutine.
	Watch(ctx context.Context, out chan<- Item) error

	// Set writes text or image content to the local system clipboard.
	// Files are NOT written to the clipboard (the daemon puts their saved
	// path on the clipboard as plain text instead), so Set may reject KindFile.
	Set(item Item) error

	// Read returns the current clipboard content (best-effort; for init/dedupe).
	Read() (Item, error)
}

// New returns the clipboard backend for the current OS.
func New() (Clipboard, error) { return newClipboard() }
