//go:build linux

package clipboard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestSelectBackend(t *testing.T) {
	cases := []struct {
		name                              string
		hasWayland, hasX11, wlPres, xPres bool
		want                              string
	}{
		{"wayland session with wl-clipboard", true, false, true, false, "wayland"},
		{"x11 session with xclip", false, true, false, true, "x11"},
		{"wayland session but only xclip installed", true, false, false, true, "x11"},
		{"x11 session but only wl installed", false, true, true, false, "wayland"},
		{"both env set, both tools -> prefer wayland", true, true, true, true, "wayland"},
		{"no env, wl present", false, false, true, false, "wayland"},
		{"no env, xclip present", false, false, false, true, "x11"},
		{"headless, nothing installed", false, false, false, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := selectBackend(c.hasWayland, c.hasX11, c.wlPres, c.xPres); got != c.want {
				t.Fatalf("selectBackend = %q, want %q", got, c.want)
			}
		})
	}
}

func TestCanonicalize(t *testing.T) {
	// X11-style atoms and Wayland-style atoms both reduce to the canonical set.
	raw := []string{"UTF8_STRING", "STRING", "TEXT", "text/plain", "text/plain;charset=utf-8", "image/png", "text/uri-list", "text/html", "TIMESTAMP"}
	got := canonicalize(raw)
	want := map[string]bool{typeText: true, typePNG: true, typeURIList: true}
	if len(got) != len(want) {
		t.Fatalf("canonicalize = %v, want the 3 canonical tokens", got)
	}
	for _, g := range got {
		if !want[g] {
			t.Fatalf("canonicalize produced unexpected token %q (got %v)", g, got)
		}
	}
}

// mockBackend is an in-memory backend used to exercise the shared shellClipboard
// logic (current/Read/Set/Watch) without any external CLI.
type mockBackend struct {
	store map[string][]byte // canonical token -> bytes
}

func newMock() *mockBackend { return &mockBackend{store: map[string][]byte{}} }

func (m *mockBackend) name() string { return "mock" }

func (m *mockBackend) listTypes(context.Context) []string {
	var out []string
	for _, t := range []string{typeURIList, typePNG, typeText} { // deterministic order
		if _, ok := m.store[t]; ok {
			out = append(out, t)
		}
	}
	return out
}

func (m *mockBackend) readType(_ context.Context, canonical string) ([]byte, error) {
	return m.store[canonical], nil
}

func (m *mockBackend) copy(_ context.Context, canonical string, data []byte) error {
	m.store = map[string][]byte{canonical: append([]byte(nil), data...)}
	return nil
}

func TestShellClipboardTextRoundTrip(t *testing.T) {
	c := &shellClipboard{b: newMock()}
	if err := c.Set(Item{Kind: KindText, Data: []byte("hello world")}); err != nil {
		t.Fatal(err)
	}
	got, err := c.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindText || string(got.Data) != "hello world" {
		t.Fatalf("Read after Set = %+v, want text 'hello world'", got)
	}
}

func TestShellClipboardImagePreferredOverText(t *testing.T) {
	m := newMock()
	m.store[typeText] = []byte("caption")
	m.store[typePNG] = []byte("\x89PNGdata")
	c := &shellClipboard{b: m}
	got, err := c.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindImage || string(got.Data) != "\x89PNGdata" {
		t.Fatalf("Read = %+v, want image preferred over text", got)
	}
}

func TestShellClipboardURIListReadsFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(fp, []byte("file contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newMock()
	m.store[typeURIList] = []byte("file://" + fp + "\n")
	c := &shellClipboard{b: m}
	got, err := c.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindFile || got.Name != "note.txt" || string(got.Data) != "file contents" {
		t.Fatalf("Read = %+v, want file note.txt with contents", got)
	}
}

// mockWatchable is a backend that reports clipboard changes via an event
// channel and counts how often its clipboard is actually read.
type mockWatchable struct {
	*mockBackend
	trigger   chan struct{}
	listCalls int32
}

func (m *mockWatchable) listTypes(ctx context.Context) []string {
	atomic.AddInt32(&m.listCalls, 1)
	return m.mockBackend.listTypes(ctx)
}

func (m *mockWatchable) watchChanges(ctx context.Context, notify func()) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.trigger:
			notify()
		}
	}
}

// Compile-time contract: the Wayland backend must be event-driven (so it never
// polls and never flickers); xclip has no event source and stays on polling.
var _ watchableBackend = waylandBackend{}

// mockUnwatchable simulates a watch backend that fails immediately, as
// wl-paste --watch does on compositors without wlr-data-control (e.g. GNOME).
type mockUnwatchable struct {
	*mockBackend
}

func (m *mockUnwatchable) watchChanges(ctx context.Context, notify func()) error {
	return fmt.Errorf("watch mode requires a compositor that supports wlroots data-control protocol")
}

// TestWatchFallsBackToPollingWhenWatchUnsupported is the regression guard for
// the "sync silently dies forever" bug: when the watchable backend's
// watchChanges fails immediately (as on GNOME/Mutter), Watch must fall back to
// polling rather than returning a fatal error that never gets retried.
func TestWatchFallsBackToPollingWhenWatchUnsupported(t *testing.T) {
	mb := newMock()
	mb.store[typeText] = []byte("still works")
	m := &mockUnwatchable{mockBackend: mb}
	c := &shellClipboard{b: m}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out := make(chan Item, 4)
	errc := make(chan error, 1)
	go func() { errc <- c.Watch(ctx, out) }()

	select {
	case it := <-out:
		if string(it.Data) != "still works" {
			t.Fatalf("emit after watch-unsupported fallback = %q, want 'still works'", it.Data)
		}
	case err := <-errc:
		t.Fatalf("Watch returned fatally instead of falling back to polling: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("Watch never fell back to polling after watchChanges failed")
	}
}

// TestWatchEventDrivenIsIdleUntilNotified is the regression guard for the
// screen-flicker bug: an event-driven backend must NOT read the clipboard while
// idle. The old polling Watch read every 300ms, which on Wayland flickered the
// whole compositor.
func TestWatchEventDrivenIsIdleUntilNotified(t *testing.T) {
	mb := newMock()
	mb.store[typeText] = []byte("copied")
	m := &mockWatchable{mockBackend: mb, trigger: make(chan struct{}, 1)}
	c := &shellClipboard{b: m}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan Item, 4)
	go c.Watch(ctx, out)

	// Idle for well over two poll intervals: a polling watcher would have read
	// the clipboard several times by now.
	time.Sleep(700 * time.Millisecond)
	if n := atomic.LoadInt32(&m.listCalls); n != 0 {
		t.Fatalf("clipboard read %d times while idle, want 0 (flicker regression)", n)
	}
	select {
	case it := <-out:
		t.Fatalf("unexpected emit while idle: %q", it.Data)
	default:
	}

	// A change notification should trigger exactly one read + emit.
	m.trigger <- struct{}{}
	select {
	case it := <-out:
		if string(it.Data) != "copied" {
			t.Fatalf("emit after change = %q, want 'copied'", it.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected an emit after a change notification")
	}
}

func TestShellClipboardWatchEmitsAndDedups(t *testing.T) {
	m := newMock()
	m.store[typeText] = []byte("first")
	c := &shellClipboard{b: m}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan Item, 4)
	go c.Watch(ctx, out)

	// First distinct value should arrive.
	select {
	case it := <-out:
		if string(it.Data) != "first" {
			t.Fatalf("first emit = %q, want 'first'", it.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first watch emit")
	}

	// Change the value; the new one should arrive (dedup lets it through).
	m.copy(ctx, typeText, []byte("second"))
	select {
	case it := <-out:
		if string(it.Data) != "second" {
			t.Fatalf("second emit = %q, want 'second'", it.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for changed watch emit")
	}
}
