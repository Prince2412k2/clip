//go:build !linux && !darwin

package clipboard

// newClipboard is the fallback for platforms without a backend (e.g. Windows,
// which is deferred past v1).
func newClipboard() (Clipboard, error) { return nil, ErrUnsupported }
