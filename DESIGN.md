# clipd — SSH-based peer-to-peer shared clipboard

**Status:** design. Nothing built yet.
**Last updated:** 2026-07-03

## Goal

A shared clipboard across a personal device fleet. Copy a screenshot, text, or
file on one machine; it streams to the others and shows up in a synced history.
Works within a text-only-over-SSH constraint (iOS Termius), on top of Tailscale.

### Stack it lives in
- Tailscale (network fabric)
- Termius on iOS
- Ghostty on mac/linux
- herdr (multiplexer) + tmux
- **Constraint:** on iOS the only thing that reliably crosses the interactive
  SSH terminal is *plain text*. No OSC 52, no file transfer, no image paste
  through the terminal.

### Decisions locked so far
- **Language/runtime:** Go. Single static binary, trivial cross-compile for
  mac + linux, good daemon story.
- **Topology:** peer-to-peer synced (each machine runs a daemon), with one node
  optionally nominated as an always-on **anchor** (star bias for healing + the
  iOS endpoint).
- **Interface:** each daemon *is* an SSH server; permission = SSH public keys.
- **Clipboard model:** history stack (ring buffer of recent entries).
- **Size cap:** 5–10 MB per entry. Under the cap → auto-streamed. Over → kept
  local, not auto-pushed.

---

## Architecture

```
  ┌─ mac ─────────┐        ┌─ linux box ───┐        ┌─ home server ─┐
  │ clipd         │◄──────►│ clipd         │◄──────►│ clipd (anchor)│
  │  watcher      │ gossip │  watcher      │ gossip │  watcher      │
  │  ssh server   │        │  ssh server   │        │  ssh server   │
  │  peer client  │        │  peer client  │        │  http door ───┼──► iOS
  │  ctrl socket  │        │  ctrl socket  │        │  ctrl socket  │
  │  history[]    │        │  history[]    │        │  history[]    │
  └───────▲───────┘        └───────────────┘        └───────────────┘
          │ ssh clip@localhost  /  ssh clip@anchor (Termius)
     you, copy / paste / browse
```

**One binary, subcommands:**
- `clipd serve` — watcher + SSH server + peer-sync client + local control socket
  (+ HTTP door on the anchor).
- `clip copy|paste|sync|anchor …` — client subcommands (thin, shell out to `ssh`
  or the local control socket).
- bare `ssh clip@anchor` — drops into the TUI history browser (for phones).

### SSH roles
- **Server side:** `charmbracelet/wish` — public-key auth middleware, PTY
  handling, and one connection can serve either a piped command *or* an
  interactive bubbletea TUI. Fallback: raw `golang.org/x/crypto/ssh`.
- **Client side:** the daemon dials each configured peer's SSH server to
  push/pull history. Same key material, mutual trust, host-key pinning.
- Bind only to the Tailscale interface (`100.x` / tailnet), never `0.0.0.0`.

### Trust
- `authorized_keys`-style allowlist; users and peers are both just pubkeys.
- Host-key pinning between peers (no MITM of gossip on a compromised node).
- Tailscale ACLs as a second gate under the SSH auth.

---

## 1. Copy anything → stream (under cap)

### Clipboard watcher (per platform)
- **macOS:** no event API — poll `NSPasteboard.changeCount` ~250 ms (cheap). On
  change read the richest type: `public.utf8-plain-text`, `public.png`/`tiff`
  (screenshots), `public.file-url` (files).
- **Linux/Wayland (ghostty):** `wl-paste --watch` gives real change events (no
  polling). Types: text, `image/png`, `text/uri-list`.
- Library shortcut: `golang.design/x/clipboard` covers text+image
  read/write/watch cross-platform — but **not files**; files are hand-rolled.

### Three payload kinds
- **Text / image** — ship the bytes.
- **File** — a "copied file" is just a path, meaningless on another machine.
  Stream *bytes + filename*. On paste the receiver either re-materializes to a
  temp file and puts that new path on its clipboard (so ⌘V in Finder works), or
  hands bytes via `clip paste > out`. **[OPEN DECISION #1]**

### Streaming mechanics
- Announce `{id, sha256, size, mime, name, origin}`. If `size ≤ cap` and receiver
  has sync on → stream bytes in chunks over the SSH channel.
- SSH is 8-bit clean, so peer/gossip traffic is **raw bytes** — base64 is only
  needed on the iOS *terminal* path (§2), nowhere else.
- **Content-address by sha256:** dedupe repeated copies and chatty watcher
  re-fires; skip re-streaming identical content.
- Over cap → keep local, optionally offer on-demand pull.

---

## 2. iOS — the honest answer

iOS **cannot run the daemon** (no background processes) and the terminal is
**text-only**. Hard physical limit: **a terminal cannot read or write the iOS
system pasteboard's image/file data — only text passes both ways.** So iOS is a
*client*, split into tiers.

### Tier 0 — text, terminal-only (works with just Termius)
- **Pull:** `ssh clip@anchor` → TUI list → pick entry → prints as text → Termius
  native long-press-select → into iOS clipboard.
- **Push:** paste text into a `clip copy` prompt.
- Needs nothing extra.

### Tier 1 — images/files to/from iOS
Requires a non-terminal channel. Plan: a **small HTTP door on the anchor, over
Tailscale (plain HTTP)**, driven by the iOS **Shortcuts** app. **[OPEN DECISION #2]**
- *Pull image:* Shortcut GETs latest entry over Tailscale → clipboard/Photos.
- *Push image:* Shortcut reads iOS clipboard image → POSTs (multipart) to anchor.
- Design the anchor so the HTTP door bolts on later without touching sync core.

> See §6 for the detailed iOS Shortcuts capability research that constrains this.

---

## 3. Sync toggle (daemon + Tailscale stay up)

- Two independent gates: **sending** and **receiving** (+ a master "pause both").
  - **receiving off:** incoming streams get a "paused" nack; local clipboard +
    history keep working.
  - **sending off:** watcher still records local copies to history, just doesn't
    push. **[OPEN DECISION #4: independent gates (rec) vs one master toggle]**
- **Control channel = local unix socket** (`$XDG_RUNTIME_DIR/clipd.sock`), not
  SSH — local admin, no network auth needed. `clip sync on|off|status`.
- State persisted to disk; survives daemon restart.
- **Payoff:** flipping receiving back on triggers an anti-entropy pull ("what did
  I miss since T?") against the anchor to catch up.

---

## 4. Anchor selection (same TUI/CLI)

- Anchor = always-on rendezvous / source-of-truth, and the iOS endpoint.
  Choosing it biases topology mesh → **star** (fewer connections, easier heal).
- TUI lists known peers; mark one as anchor. `clip anchor set <host>`.
- **[OPEN DECISION #3]** anchor scope:
  - **Global** synced setting (change on any node → gossips, wins by timestamp,
    everyone agrees; unambiguous iOS endpoint) — probably what we want.
  - **Per-node** preference (simpler, but nodes can disagree where iOS points).

---

## 5. Sync protocol

Each entry: unique id (ULID-ish), `sha256`, `ts`, `origin_host`, `mime`, bytes.

- **Last-writer-wins gossip:** on copy, push to peers; each merges by id, sorts
  by ts, truncates to N. Simple; clock skew is the only hazard (tailnet hosts are
  NTP-synced, so minor).
- **Anti-entropy pull:** periodically (and on receive-resume) ask peers "ids
  since T?" and backfill. Heals missed pushes while a laptop/phone slept —
  important given Mac/iOS sleep constantly.
- Full CRDT/vector clocks: overkill, skip.

### History + cap
- Ring buffer of last N entries (~50).
- Per-entry cap 5–10 MB; also bound total buffer memory (~64 MB), evict oldest.

---

## 6. iOS Shortcuts capability research (2024–2026, iOS 17/18/26)

Findings that constrain the Tier-1 iOS bridge. Confidence: **[H]/[M]/[L]**.
❌ = hard limit.

### Bottom-line hard limits
1. **iOS is client-only.** No inbound SSH server, no headless remote trigger, no
   clipboard-change trigger. Every "push to phone" must be a phone-initiated
   **pull** (manual/automation trigger). **[H]**
2. **No true background.** Shortcuts must be foreground to do HTTP/SSH; runtime
   budget ~25 s. Unattended needs an always-on helper (Mac/Pi) or Pushcut. **[H]**
3. **TLS is strict** — no "ignore certificate" toggle; plain self-signed is
   rejected. → **Use plain HTTP over the Tailscale tunnel** (simplest), or
   Tailscale `*.ts.net` Let's Encrypt certs, or an installed+trusted private CA. **[H]**
4. **Images work end-to-end** — read from clipboard, base64 or multipart upload,
   download raw bytes back to clipboard/Photos. Prefer **raw-bytes + multipart
   Form** over base64-in-JSON. **[H]**
5. **SSH from Shortcuts works natively BUT** the private key is app-generated and
   **non-importable**; **Termius is a dead end for output capture.** **[H]**

### Clipboard access
- `Get Clipboard` reads any type (text/URL/image/file), UTI auto-detected. **[H]**
- Reading a clipboard image's bytes: **yes** — `Get Clipboard` → `Base64 Encode`
  (MacStories "Encode Images to Base64"). **[H]**
- `Copy to Clipboard` writes any type incl. images (JPG may normalize to PNG).
  Params: `Local Only` (block Universal Clipboard), `Expire At`. **[H]**
- ❌ No documented size cap; big blobs + Universal Clipboard cause sync lockups →
  set `Local Only`. **[M]**

### Network — `Get Contents of URL`
- Methods GET/POST/PUT/PATCH/DELETE; ❌ no custom method string. **[H]**
- Custom headers: arbitrary key/values. **[H]**
- Body: **JSON** (❌ no top-level array — build string, send as File), **Form**
  (`x-www-form-urlencoded`; a field typed file+fileName → multipart, the reliable
  **binary upload** path), **File** (raw binary body). **[H]**
- Binary responses: **yes** — typed from Content-Type; then Save to Photos / Save
  File / Copy to Clipboard. **[H]**
- ❌ No built-in auth field — do `Authorization` header manually (Bearer / Basic
  with self-base64'd creds / custom API key). **[H]**
- ❌ Timeout not configurable, ~25 s (some report ~60 s). **[M]**
- ❌ TLS: no insecure toggle; self-signed rejected (see hard-limit #3). **[H]**

**Tailscale from iPhone:** reaches `100.x` and `host.tailnet.ts.net` once tunnel
up (all apps route 100.x via VPN). **[M]** — inferred from VPN routing, not a
documented Shortcuts test. **Plain HTTP to `100.x` avoids the cert problem;
prefer the raw 100.x IP over MagicDNS in automations.** **[H/M]**

**Foreground/background:** ❌ Shortcuts effectively must be foreground for HTTP
(no background URLSession); ~30 s grace on resume, killed if swiped away. Keep it
foreground, well under ~25 s. **[H]**

### SSH from Shortcuts
- Native **`Run Script Over SSH`** (since iOS 13; keys later). Password + SSH key;
  captures stdout. **[H]**
- ❌ **Cannot import your own private key** — the action only *generates* a
  keypair (ed25519/RSA); you add the generated **public** key to the daemon's
  `authorized_keys`. Private key non-exportable. **[H]**
- ❌ Binary output unconfirmed (treated as text); host-key behavior undocumented. **[L]**
- ⚠️ On a locked device via automation, SSH action may wait for a tap — not truly
  headless. **[M]**
- **Termius:** ❌ no usable Shortcuts scripting integration (Siri "connect/run
  snippet" launch only, no stdout return, no public URL scheme). **[H]**
- **Viable terminal apps:** **a-Shell** (Execute Command returns output; bundles
  ssh/scp/curl/python3; can run headless "In Extension"), **Secure ShellFish**
  (Run Command/Upload/Download; real key control incl. Secure Enclave/YubiKey;
  Files-app SFTP). Blink/Prompt/iSH — not suitable (no stdout return). **[H]**

### Encoding / files
- `Base64 Encode` (+ Decode toggle) since iOS 13; works on text/images/files. **[H]**
- ❌ **`Line Breaks` defaults ON (break every 76 chars) → corrupts payloads.**
  Turn it **OFF**. #1 gotcha. **[H]**
- 5–10 MB base64: ❌ no official limit; ~33% inflation, heavy memory/slow; the
  "5 MB" figure floating around is a web-embedding guideline, not a Shortcuts
  limit. **[M]**
- Files: `Save File` / `Get File` / `Get Contents of File` / `Delete Files`.
  ❌ No temp-file API — scratch files go in the iCloud Shortcuts folder, then
  `Delete Files`. **[H]/[M]**
- Image → base64 → HTTP body confirmed working; ❌ no on-device shell, so
  "command argument" locally = a variable into an action param. **Prefer HTTP
  File/Form body over base64-in-JSON for images** (avoids length/line-wrap). **[H]**

### Automation / triggers
- ❌ **No clipboard-change trigger** in iOS 17/18/26. Clipboard only read *inside*
  an otherwise-triggered shortcut. **[H]**
- Manual surfaces: Share Sheet, Home Screen/widget, Back Tap, Action Button,
  Control Center, Siri, Spotlight, Watch, `shortcuts://` URL. **[H]**
- Auto triggers: time/alarm/sleep/arrive/leave/CarPlay/Wi-Fi/BT/NFC/app/focus/
  battery/charger/email/message/… iOS 26 added Notification-received (keyword),
  Screenshot-taken, Keyboard-connected. **[H]**
- Background: since iOS 17 most automations can "Run Immediately," but some always
  notify/confirm; 18.x had regressions. ❌ **No inbound headless trigger** — phone
  is a client, never remotely commandable. **[H/M]**

### Practical patterns
- **Push/pull text over HTTP:** one `Get Contents of URL`. Push = `Get Clipboard`
  → POST JSON `{"text": …}`. Pull = GET → `Get Dictionary Value` →
  `Copy to Clipboard`. Shared secret in `Authorization` header. **[H]**
- **Share-sheet image → POST:** Input → `Get Contents of URL` POST, **Form** body
  `file`=image + `fileName`. (base64+JSON only if server insists; Line Breaks
  OFF.) **[H]**
- **HTTP GET image → clipboard/Photos:** server returns **raw bytes with correct
  Content-Type** → `Get Contents of URL` yields Image directly → Save/Copy.
  Returning raw bytes avoids base64-decode type-loss. **[H]**
- **Closest prior art:** `copypasteengine/clipboard-bridge` — Go service, no
  native iOS app, ships **Shortcuts** support (HTTP REST push/pull + token). Good
  architectural model. **[H]**

### Softest points (verify on a real device before finalizing)
Tailscale-over-Shortcuts (inferred), exact HTTP timeout (~25 vs ~60 s), base64
crash threshold, native SSH binary-output / host-key behavior, locked-device
automation reliability.

### Implication for our design
- **iOS Tier-1 = plain HTTP door on the anchor over Tailscale (100.x IP), token
  in an `Authorization` header.** Not HTTPS (TLS too strict), not Termius SSH
  (no stdout), not native `Run Script Over SSH` for images (text/stdout + key
  friction). Images: multipart Form upload; download as raw bytes.
- iOS is **pull-only, foreground, <25 s per op, no clipboard-change trigger** —
  so "new copy → auto-appears on phone" is impossible; the phone pulls on demand.

---

## Open decisions (blockers before build)

1. **Pasted files** on Mac/Linux — re-materialize as real files (⌘V-able) or
   CLI-retrieval only?
2. **iOS images** — build the Tailscale HTTP door + Shortcuts, or text-from-iOS
   only (Tier 0)?
3. **Anchor scope** — global synced setting or per-node preference?
4. **Send/receive gates** — independent (rec) or one master toggle?

## Recommended default (least fragile v1)
P2P protocol with the home server nominated as always-on anchor; static peer
config; LWW gossip + periodic/anti-entropy pull; single Go binary
(wish + bubbletea); Tier-0 iOS text now, HTTP door designed-for-later.
