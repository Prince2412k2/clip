//go:build darwin

package clipboard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// pollInterval is how often Watch polls the pasteboard for changes. macOS
// exposes no change-notification API without cgo (NSPasteboard.changeCount
// requires linking AppKit), so we poll instead.
const pollInterval = 300 * time.Millisecond

// darwinClipboard shells out to pbcopy/pbpaste/osascript so the backend stays
// cgo-free and cross-compilable.
type darwinClipboard struct{}

func newClipboard() (Clipboard, error) { return darwinClipboard{}, nil }

// Watch polls the pasteboard and emits an Item whenever its content changes.
func (darwinClipboard) Watch(ctx context.Context, out chan<- Item) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var lastHash [32]byte
	haveLast := false

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			item, err := readClipboard(ctx)
			if err != nil || item == nil {
				continue // empty or unreadable: nothing to emit
			}

			h := sha256.Sum256(item.Data)
			if haveLast && h == lastHash {
				continue // unchanged
			}
			lastHash = h
			haveLast = true

			select {
			case out <- *item:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// Read returns the current richest clipboard content.
func (darwinClipboard) Read() (Item, error) {
	item, err := readClipboard(context.Background())
	if err != nil {
		return Item{}, err
	}
	if item == nil {
		return Item{}, errors.New("clipboard: empty")
	}
	return *item, nil
}

// Set writes text or image content to the system clipboard. Files are not
// supported since Set only ever writes pasteboard-representable content.
func (darwinClipboard) Set(item Item) error {
	switch item.Kind {
	case KindText:
		cmd := exec.Command("pbcopy")
		cmd.Stdin = bytes.NewReader(item.Data)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("clipboard: pbcopy: %w (%s)", err, bytes.TrimSpace(out))
		}
		return nil

	case KindImage:
		tmp, err := os.CreateTemp("", "clippy-set-*.png")
		if err != nil {
			return fmt.Errorf("clipboard: create temp file: %w", err)
		}
		tmpPath := tmp.Name()
		defer os.Remove(tmpPath)

		if _, err := tmp.Write(item.Data); err != nil {
			tmp.Close()
			return fmt.Errorf("clipboard: write temp file: %w", err)
		}
		if err := tmp.Close(); err != nil {
			return fmt.Errorf("clipboard: close temp file: %w", err)
		}

		script := fmt.Sprintf(`set the clipboard to (read (POSIX file %q) as «class PNGf»)`, tmpPath)
		cmd := exec.Command("osascript", "-e", script)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("clipboard: osascript set image: %w (%s)", err, bytes.TrimSpace(out))
		}
		return nil

	case KindFile:
		return errors.New("clipboard: Set does not support KindFile")

	default:
		return fmt.Errorf("clipboard: unsupported item kind %q", item.Kind)
	}
}

// readClipboard detects and reads the richest content currently on the
// pasteboard, checking in order: file > image (PNG) > text. It returns
// (nil, nil) when the clipboard is empty or holds nothing we recognize.
func readClipboard(ctx context.Context) (*Item, error) {
	if item, err := readClipboardFile(ctx); err != nil {
		return nil, err
	} else if item != nil {
		return item, nil
	}

	if item, err := readClipboardImage(ctx); err != nil {
		return nil, err
	} else if item != nil {
		return item, nil
	}

	return readClipboardText(ctx)
}

// readClipboardFile detects a local file URL on the pasteboard (e.g. copied
// from Finder) and reads its bytes. Returns (nil, nil) if there is no file
// URL on the clipboard.
func readClipboardFile(ctx context.Context) (*Item, error) {
	const script = `POSIX path of (the clipboard as «class furl»)`
	out, err := runOsascript(ctx, script)
	if err != nil {
		// AppleScript errors when the clipboard holds no file URL; treat as
		// "not a file" rather than a fatal error.
		return nil, nil
	}

	path := string(bytes.TrimSpace(out))
	if path == "" {
		return nil, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		// Unreadable path (permissions, gone, etc): skip, not fatal.
		return nil, nil
	}

	mime := mimeFromExt(filepath.Ext(path))
	return &Item{
		Kind: KindFile,
		Mime: mime,
		Name: filepath.Base(path),
		Data: data,
	}, nil
}

// readClipboardImage reads a PNG representation of the pasteboard content, if
// any. Returns (nil, nil) if the clipboard holds no image.
func readClipboardImage(ctx context.Context) (*Item, error) {
	tmp, err := os.CreateTemp("", "clippy-get-*.png")
	if err != nil {
		return nil, fmt.Errorf("clipboard: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	script := fmt.Sprintf(`set thePng to (the clipboard as «class PNGf»)
set fh to open for access POSIX file %q with write permission
set eof fh to 0
write thePng to fh
close access fh`, tmpPath)

	if _, err := runOsascript(ctx, script); err != nil {
		// AppleScript errors when the clipboard holds no image data; treat as
		// "not an image" rather than a fatal error.
		return nil, nil
	}

	data, err := os.ReadFile(tmpPath)
	if err != nil || len(data) == 0 {
		return nil, nil
	}

	return &Item{Kind: KindImage, Mime: "image/png", Data: data}, nil
}

// readClipboardText reads the pasteboard as plain text via pbpaste. Returns
// (nil, nil) if the clipboard is empty.
func readClipboardText(ctx context.Context) (*Item, error) {
	cmd := exec.CommandContext(ctx, "pbpaste")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// pbpaste exits non-zero when the clipboard has no text
		// representation; treat as empty rather than fatal.
		return nil, nil
	}

	if stdout.Len() == 0 {
		return nil, nil
	}

	return &Item{Kind: KindText, Mime: "text/plain", Data: stdout.Bytes()}, nil
}

// runOsascript runs an AppleScript snippet and returns its stdout, or an
// error if the script failed (including AppleScript runtime errors, which
// callers here use to mean "no such pasteboard representation").
func runOsascript(ctx context.Context, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "osascript", "-e", script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("osascript: %w (%s)", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}

// mimeFromExt returns a best-effort MIME type for a file extension.
func mimeFromExt(ext string) string {
	switch ext {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".pdf":
		return "application/pdf"
	case ".txt":
		return "text/plain"
	default:
		return "application/octet-stream"
	}
}
