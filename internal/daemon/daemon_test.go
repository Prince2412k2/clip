package daemon

import (
	"context"
	"fmt"
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
	mu     sync.Mutex
	sets   []clipboard.Item
	setErr error
}

func (f *fakeClip) Watch(ctx context.Context, out chan<- clipboard.Item) error {
	<-ctx.Done()
	return ctx.Err()
}
func (f *fakeClip) Set(it clipboard.Item) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets = append(f.sets, it)
	return f.setErr
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
	sender = func(ctx context.Context, addr string, p *payload.Payload) error {
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

// FR-006 for the file/image path: the clipboard now holds the saved file's
// PATH as plain text, not its raw bytes, so the watcher observes the path
// string — echo suppression must be keyed on that, not on the original body.
func TestFileReceiveEchoSuppression(t *testing.T) {
	d, _, sends := testDaemon(t)

	st, err := d.Receive("file", "notes.txt", "text/plain", "", []byte("file contents"))
	if st != 200 || err != nil {
		t.Fatalf("Receive: status=%d err=%v", st, err)
	}
	dest := filepath.Join(d.cfg.RecvDir, "notes.txt")

	// The watcher observes the clipboard's new content: the path string, not
	// the file's original bytes. Must be swallowed, not re-sent.
	d.onLocalChange(context.Background(), clipboard.Item{Kind: clipboard.KindText, Mime: "text/plain", Data: []byte(dest)})
	if *sends != 0 {
		t.Fatalf("echo was re-sent: sends=%d, want 0", *sends)
	}
}

// FR-002 + FR-005: a genuinely new copy is sent once; an identical repeat is
// deduped. Uses an image, since local text copies are never auto-sent (see
// TestLocalTextNeverAutoSent).
func TestSendAndDedupe(t *testing.T) {
	d, _, sends := testDaemon(t)
	item := clipboard.Item{Kind: clipboard.KindImage, Mime: "image/png", Data: []byte("fresh copy")}

	d.onLocalChange(context.Background(), item)
	if *sends != 1 {
		t.Fatalf("new copy: sends=%d, want 1", *sends)
	}
	d.onLocalChange(context.Background(), item) // identical repeat
	if *sends != 1 {
		t.Fatalf("dedupe failed: sends=%d, want 1", *sends)
	}
}

func TestLocalImageSavedAndCopiedToContainer(t *testing.T) {
	d, fc, sends := testDaemon(t)
	d.cfg.DockerContainer = "my-devbox"

	var gotContainer, gotPath string
	origCopy := copyToContainer
	copyToContainer = func(container, path string) error {
		gotContainer, gotPath = container, path
		return nil
	}
	t.Cleanup(func() { copyToContainer = origCopy })

	d.onLocalChange(context.Background(), clipboard.Item{
		Kind: clipboard.KindImage,
		Mime: "image/png",
		Data: []byte("screenshot"),
	})

	dest := filepath.Join(d.cfg.RecvDir, "clippy-image.png")
	contents, err := os.ReadFile(dest)
	if err != nil || string(contents) != "screenshot" {
		t.Fatalf("saved screenshot = %q, err=%v", contents, err)
	}
	if gotContainer != "my-devbox" || gotPath != dest {
		t.Fatalf("copyToContainer(%q, %q), want (%q, %q)", gotContainer, gotPath, "my-devbox", dest)
	}
	if len(fc.sets) != 1 || string(fc.sets[0].Data) != dest {
		t.Fatalf("clipboard Set = %+v, want path %q", fc.sets, dest)
	}
	if *sends != 1 {
		t.Fatalf("remote sends=%d, want 1", *sends)
	}
}

func TestLocalImageSavedWhenSyncOff(t *testing.T) {
	d, fc, sends := testDaemon(t)
	if err := d.SetSync(false); err != nil {
		t.Fatal(err)
	}
	d.onLocalChange(context.Background(), clipboard.Item{
		Kind: clipboard.KindImage,
		Mime: "image/png",
		Data: []byte("offline screenshot"),
	})

	dest := filepath.Join(d.cfg.RecvDir, "clippy-image.png")
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("local screenshot not saved with sync off: %v", err)
	}
	if len(fc.sets) != 1 || string(fc.sets[0].Data) != dest {
		t.Fatalf("clipboard Set = %+v, want path %q", fc.sets, dest)
	}
	if *sends != 0 {
		t.Fatalf("remote sends=%d, want 0", *sends)
	}
}

// A local text copy is never auto-sent outward, regardless of sync state:
// this fleet only pushes text phone -> PC, not PC -> peers. Files and images
// still sync out (see TestSendAndDedupe).
func TestLocalTextNeverAutoSent(t *testing.T) {
	d, _, sends := testDaemon(t)
	d.onLocalChange(context.Background(), clipboard.Item{Kind: clipboard.KindText, Data: []byte("typed on the pc")})
	if *sends != 0 {
		t.Fatalf("local text copy was sent: sends=%d, want 0", *sends)
	}
}

// FR-012: with sync off, nothing is sent.
func TestSyncGate(t *testing.T) {
	d, _, sends := testDaemon(t)
	if err := d.SetSync(false); err != nil {
		t.Fatalf("SetSync: %v", err)
	}
	d.onLocalChange(context.Background(), clipboard.Item{Kind: clipboard.KindImage, Mime: "image/png", Data: []byte("nope")})
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
	d, fc, _ := testDaemon(t)

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

	if len(fc.sets) != 2 ||
		fc.sets[0].Kind != clipboard.KindText || string(fc.sets[0].Data) != filepath.Join(dir, "notes.txt") ||
		fc.sets[1].Kind != clipboard.KindText || string(fc.sets[1].Data) != filepath.Join(dir, "notes (1).txt") {
		t.Fatalf("clipboard Set calls = %+v, want plain-text absolute paths for both saved files in order", fc.sets)
	}
}

// A received file must still be saved and report 200 even if copying its path
// to the clipboard fails (e.g. no display session) — disk delivery is the
// guarantee; clipboard integration is best-effort on top of it.
func TestFileReceiveClipboardCopyIsBestEffort(t *testing.T) {
	d, fc, _ := testDaemon(t)
	fc.setErr = fmt.Errorf("no display session")

	st, err := d.Receive("file", "notes.txt", "text/plain", "", []byte("content"))
	if st != 200 || err != nil {
		t.Fatalf("Receive: status=%d err=%v, want 200/nil even when clipboard copy fails", st, err)
	}

	data, err := os.ReadFile(filepath.Join(d.cfg.RecvDir, "notes.txt"))
	if err != nil || string(data) != "content" {
		t.Fatalf("file not saved to disk despite clipboard failure: %q err=%v", data, err)
	}
	if len(fc.sets) != 1 {
		t.Fatalf("clipboard Set not attempted: %v", fc.sets)
	}
}

// An image received with no filename (e.g. synced from a peer's system
// clipboard, which carries no name) gets a mime-derived extension and its
// saved path — not raw image bytes — placed on the clipboard.
func TestImageReceiveNoNameGetsExtensionAndPathOnClipboard(t *testing.T) {
	d, fc, _ := testDaemon(t)

	st, err := d.Receive("image", "", "image/png", "", []byte("png bytes"))
	if st != 200 || err != nil {
		t.Fatalf("Receive: status=%d err=%v", st, err)
	}

	dir := d.cfg.RecvDir
	dest := filepath.Join(dir, "clippy-image.png")
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "png bytes" {
		t.Fatalf("clippy-image.png: %q err=%v", data, err)
	}
	if len(fc.sets) != 1 || fc.sets[0].Kind != clipboard.KindText || string(fc.sets[0].Data) != dest {
		t.Fatalf("clipboard Set = %+v, want plain-text path %q", fc.sets, dest)
	}
}

// When a docker container is configured, a received file/image is docker-cp'd
// into it at the same absolute path.
func TestFileReceiveCopiesToConfiguredContainer(t *testing.T) {
	d, _, _ := testDaemon(t)
	d.cfg.DockerContainer = "my-devbox"

	var gotContainer, gotPath string
	origCopy := copyToContainer
	copyToContainer = func(container, path string) error {
		gotContainer, gotPath = container, path
		return nil
	}
	t.Cleanup(func() { copyToContainer = origCopy })

	if st, err := d.Receive("file", "notes.txt", "text/plain", "", []byte("hi")); st != 200 || err != nil {
		t.Fatalf("Receive: status=%d err=%v", st, err)
	}

	wantPath := filepath.Join(d.cfg.RecvDir, "notes.txt")
	if gotContainer != "my-devbox" || gotPath != wantPath {
		t.Fatalf("copyToContainer(%q, %q), want (%q, %q)", gotContainer, gotPath, "my-devbox", wantPath)
	}
}

// With no docker container configured, copyToContainer must never be invoked.
func TestFileReceiveSkipsDockerCopyWhenUnconfigured(t *testing.T) {
	d, _, _ := testDaemon(t)

	called := false
	origCopy := copyToContainer
	copyToContainer = func(container, path string) error {
		called = true
		return nil
	}
	t.Cleanup(func() { copyToContainer = origCopy })

	if st, err := d.Receive("file", "notes.txt", "text/plain", "", []byte("hi")); st != 200 || err != nil {
		t.Fatalf("Receive: status=%d err=%v", st, err)
	}
	if called {
		t.Fatal("copyToContainer was called despite no docker_container configured")
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
