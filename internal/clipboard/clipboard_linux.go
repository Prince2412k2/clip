//go:build linux

package clipboard

// TODO(agent A): replace this stub with a wl-clipboard backend
// (wl-paste --watch for events; typed reads for text, image/png, and
// text/uri-list -> read file bytes; wl-copy to Set).
func newClipboard() (Clipboard, error) { return nil, ErrUnsupported }
