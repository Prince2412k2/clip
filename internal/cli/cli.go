// Package cli implements the client subcommands (peers, target, on, off,
// status) as a thin HTTP client to the local daemon.
package cli

import "errors"

// Run dispatches a client subcommand. args[0] is the subcommand name.
//
// TODO(agent F): implement peers | target <name> | on | off | status by
// calling the local daemon's /v1 API (see internal/transport/api.go).
func Run(args []string) error {
	return errors.New("cli: not implemented")
}
