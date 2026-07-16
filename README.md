# clippy

Targeted cross-platform clipboard for a personal [Tailscale](https://tailscale.com)
fleet. Copy on one machine, and it lands on a **selected target machine's**
clipboard — text, images, and files up to 20 MB. A small phone-facing webapp lets
you drop text/files/images to any machine from a browser.

> v1 scope and decisions live in `docs/specs/2026-07-13-clippy-v1.md`; the plan in
> `docs/plans/2026-07-13-clippy-v1.md`. `DESIGN.md` is the longer-term north star
> (P2P gossip, history, TUI, iOS Shortcuts) — v1 deliberately uses the simpler
> targeted-send + unified-HTTP model instead.

## How it works

Each machine runs `clippy serve`: a clipboard **watcher** + an **HTTP server**
bound to the Tailscale interface (never `0.0.0.0`). When your clipboard changes,
the daemon pushes it to the currently selected target's `/v1/clip`. The target
writes text/images straight to its system clipboard and saves files into a
receive folder. The same HTTP server hosts the phone webapp.

There is no application-level auth: the daemon listens only on the Tailscale
interface, so Tailscale's encryption and ACLs are the trust boundary. Anyone on
your tailnet can write to the clipboard — keep the tailnet to machines you trust.

```
  copy on A ─▶ watcher ─▶ POST /v1/clip ─▶ B sets clipboard / saves file
  phone ─▶ webapp ─▶ POST /v1/send (pick target) ─▶ relay ─▶ target
```

Correctness invariants (unit-tested in `internal/daemon`): content received and
written to the clipboard is **not** echoed back (no A→B→A loop), and identical
repeats are deduped by content hash.

## Build

Single static binary, no cgo, stdlib only.

```sh
go build -o clippy ./cmd/clippy
# cross-compile:
GOOS=darwin go build -o clippy-mac ./cmd/clippy
```

Requires at runtime: `tailscale` (peer discovery); on Linux either
`wl-clipboard` (`wl-copy`/`wl-paste`, Wayland) or `xclip` (X11) — the backend is
auto-detected from `WAYLAND_DISPLAY`/`DISPLAY` and whichever tool is installed;
on macOS the built-in `pbcopy`/`pbpaste`/`osascript`.

## Use

Run the daemon on every machine — no shared secret to provision:

```sh
clippy serve            # run the daemon (watcher + HTTP + webapp)
clippy peers            # list tailnet machines
clippy target <name>    # choose where your copies go
clippy target            # ...or arrow-key through peers inline, Enter to pick
clippy docker             # pick a local Docker container inline
clippy docker off         # disable container copying
clippy on | off         # enable / disable sending
clippy status           # sync state, target, address
```

Phone: browse to `http://<machine-tailnet-ip>:8787/`, pick a target, and drop
text/files/images.

Config keys (`config.json`): `target`, `sync_enabled`, `recv_dir` (default
`~/Downloads/clippy`), `port` (default 8787), `docker_container` (default "" =
disabled). Set `CLIPPY_CONFIG` to use an alternate path (handy for running two
daemons on one host).

A received file or image is saved to `recv_dir`, then, if `docker_container` is
set, `docker cp`'d into that container at the same absolute path (its parent
directory is created inside the container first). Either way, the saved path
is put on the clipboard as plain text — pasteable anywhere, including into a
terminal or a containerized TUI (e.g. `claude`/`codex` running in a headless
dev container with no display access of its own).

## Verification status

Verified on this Linux host: build on linux/darwin/windows, `go vet`, daemon and
transport unit tests, and a live server run — the `/v1/status|target|sync|peers`
control API (no auth; tailnet-only), CLI↔daemon control, file receive to the recv
folder, config persistence across restart, tailnet peer enumeration against a real
`tailscaled`, and the webapp serving at `/`.

**Not yet exercised end-to-end** (no live Wayland compositor on the test host, and
no second peer): actual text/image clipboard read+write via `wl-clipboard`, the
poll-based watcher firing on a real copy, the full two-machine send loop, and the
entire macOS backend (cross-compiles, but AppleScript paths are unrun). These need
a real desktop session / a second fleet machine to validate.

## Not in v1

P2P gossip / multi-peer fan-out, clipboard history & TUI, iOS Shortcuts tiers,
anchor nomination, a Windows clipboard backend, service install/packaging, HTTPS
(the link is encrypted by Tailscale).
