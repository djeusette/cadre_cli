// Package cli dispatches cadre's commands.
package cli

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

const usage = "Usage: cadre <login|whoami|project create <file>|version> [--json]"

// Env carries everything a command touches outside its arguments, so tests can fake it.
type Env struct {
	Stdout, Stderr io.Writer
	Getenv         func(string) string
	OpenBrowser    func(string) error
	Sleep          func(time.Duration)
	HTTP           *http.Client
	Version        string
}

// Run executes the command in args and returns the exit code: 0 ok, 1 failure, 2 usage error.
func Run(args []string, env Env) int {
	if len(args) == 0 {
		return usageError(env)
	}

	switch args[0] {
	case "version":
		fmt.Fprintf(env.Stdout, "cadre %s\n", env.Version)
		return 0
	default:
		return usageError(env)
	}
}

func usageError(env Env) int {
	fmt.Fprintln(env.Stderr, usage)
	return 2
}
