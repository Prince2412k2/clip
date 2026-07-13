//go:build darwin

package clipboard

// TODO(agent B): replace this stub with an NSPasteboard backend that shells out
// to pbpaste/pbcopy/osascript (poll changeCount ~250ms; text, PNG image, and
// file-URL; no cgo).
func newClipboard() (Clipboard, error) { return nil, ErrUnsupported }
