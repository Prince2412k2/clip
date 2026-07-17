// Command clippy is a targeted cross-platform clipboard sender.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"clippy/internal/cli"
	"clippy/internal/clipboard"
	"clippy/internal/config"
	"clippy/internal/daemon"
	"clippy/internal/transport"
	"clippy/internal/webapp"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		if err := serve(); err != nil {
			fatal(err)
		}
	case "peers", "target", "docker", "on", "off", "status":
		if err := cli.Run(os.Args[1:]); err != nil {
			fatal(err)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "clippy: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func serve() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	clip, err := clipboard.New()
	if err != nil {
		return err
	}
	d := daemon.New(cfg, clip)
	srv := transport.NewServer(cfg, d, webapp.Handler())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 2)
	go func() { errCh <- srv.Run(ctx) }()
	go func() { errCh <- d.Run(ctx) }()

	fmt.Printf("clippy serving on port %d (sync=%v target=%q)\n", cfg.Port, cfg.SyncEnabled, cfg.Target)
	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "clippy:", err)
	os.Exit(1)
}

func usage() {
	fmt.Fprint(os.Stderr, `clippy — targeted cross-platform clipboard

usage:
  clippy serve            run the daemon (watcher + HTTP server + webapp)
  clippy peers            list tailnet machines
  clippy target <name>    set the target machine
  clippy docker [name]    choose a local Docker container ("off" disables)
  clippy on | off         enable / disable sending
  clippy status           show sync state, target, and address
`)
}
