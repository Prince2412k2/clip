package daemon

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"clippy/internal/clipboard"
	"clippy/internal/config"
	"clippy/internal/payload"
)

// fakeClip records Set calls and never blocks.
type fakeClip struct {
	mu   sync.Mutex
	sets []clipboard.Item
}

func (f *fakeClip) Watch(ctx context.Context, out chan<- clipboard.Item) error {
	<-ctx.Done()
	return ctx.Err()
}
func (f *fakeClip) Set(it clipboard.Item) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets = append(f.sets, it)
	return nil
}
func (f *fakeClip) Read() (clipboard.Item, error) { return clipboard.Item{}, nil }

// testDaemon builds a daemon with a temp config (target = a literal IP so
// tailnet.Resolve short-circuits without touching the network) and a recorder
// in place of the real sender. It returns the daemon and a pointer to the send
// counter.
func testDaemon(t *testing.T) (*Daemon, *fakeClip, *int) {
	t.Helper()
	t.Setenv("CLIPPY_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Target = "100.64.0.1" // parses as IP -> Resolve needs no tailscale
	cfg.SyncEnabled = true
	cfg.RecvDir = filepath.Join(t.TempDir(), "inbox")

	fc := &fakeClip{}
	d := New(cfg, fc)

	sends := 0
	orig := sender
	sender = func(ctx context.Context, token, addr string, p *payload.Payload) error {
		sends++
		return nil
	}
	t.Cleanup(func() { sender = orig })
	return d, fc, &sends
}

// FR-006: content received and written to the clipboard must NOT be re-sent by
// the watcher (no A->B->A echo).
func TestEchoSuppression(t *testing.T) {
	d, _, sends := testDaemon(t)
	body := []byte("hello world")

	if st, err := d.Receive("text", "", "text/plain", payload.Hash(body), body); st != 200 || err != nil {
		t.Fatalf("Receive: status=%d err=%v", st, err)
	}
	// The watcher observes the very content we just set — must be swallowed.
	d.onLocalChange(context.Background(), clipboard.Item{Kind: clipboard.KindText, Mime: "text/plain", Data: body})
	if *sends != 0 {
		t.Fatalf("echo was re-sent: sends=%d, want 0", *sends)
	}
}

// FR-002 + FR-005: a genuinely new copy is sent once; an identical repeat is
// deduped.
func TestSendAndDedupe(t *testing.T) {
	d, _, sends := testDaemon(t)
	item := clipboard.Item{Kind: clipboard.KindText, Mime: "text/plain", Data: []byte("fresh copy")}

	d.onLocalChange(context.Background(), item)
	if *sends != 1 {
		t.Fatalf("new copy: sends=%d, want 1", *sends)
	}
	d.onLocalChange(context.Background(), item) // identical repeat
	if *sends != 1 {
		t.Fatalf("dedupe failed: sends=%d, want 1", *sends)
	}
}

// FR-012: with sync off, nothing is sent.
func TestSyncGate(t *testing.T) {
	d, _, sends := testDaemon(t)
	if err := d.SetSync(false); err != nil {
		t.Fatalf("SetSync: %v", err)
	}
	d.onLocalChange(context.Background(), clipboard.Item{Kind: clipboard.KindText, Data: []byte("nope")})
	if *sends != 0 {
		t.Fatalf("sent while sync off: sends=%d, want 0", *sends)
	}
}

// FR-004: content over the cap is never sent.
func TestOverCapNotSent(t *testing.T) {
	d, _, sends := testDaemon(t)
	big := make([]byte, payload.MaxSize+1)
	d.onLocalChange(context.Background(), clipboard.Item{Kind: clipboard.KindImage, Mime: "image/png", Data: big})
	if *sends != 0 {
		t.Fatalf("over-cap payload was sent: sends=%d, want 0", *sends)
	}
}

// FR-008: received files are saved to the recv dir and never overwrite an
// existing file of the same name.
func TestFileReceiveDeCollide(t *testing.T) {
	d, _, _ := testDaemon(t)

	if st, err := d.Receive("file", "notes.txt", "text/plain", "", []byte("first")); st != 200 || err != nil {
		t.Fatalf("Receive 1: status=%d err=%v", st, err)
	}
	if st, err := d.Receive("file", "notes.txt", "text/plain", "", []byte("second")); st != 200 || err != nil {
		t.Fatalf("Receive 2: status=%d err=%v", st, err)
	}

	dir := d.cfg.RecvDir
	first, err := os.ReadFile(filepath.Join(dir, "notes.txt"))
	if err != nil || string(first) != "first" {
		t.Fatalf("notes.txt: %q err=%v", first, err)
	}
	second, err := os.ReadFile(filepath.Join(dir, "notes (1).txt"))
	if err != nil || string(second) != "second" {
		t.Fatalf("notes (1).txt: %q err=%v", second, err)
	}
}

// FR-004 receive side: an oversize inbound body is rejected with 413.
func TestReceiveOverCap(t *testing.T) {
	d, _, _ := testDaemon(t)
	big := make([]byte, payload.MaxSize+1)
	if st, _ := d.Receive("text", "", "text/plain", "", big); st != 413 {
		t.Fatalf("over-cap receive: status=%d, want 413", st)
	}
}
