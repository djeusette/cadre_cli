// Package cli dispatches cadre's commands.
package cli

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

const usage = "Usage: cadre <login --url <url>|whoami|project create <file>|spec pull <project>/<n> [-o <file>]|spec push <project>/<n> <file> [--base <version>]|version> [--json]"

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
	args, asJSON := splitJSONFlag(args)
	if len(args) == 0 {
		return usageError(env)
	}

	switch args[0] {
	case "version":
		fmt.Fprintf(env.Stdout, "cadre %s\n", env.Version)
		return 0
	case "login":
		return runLogin(env, asJSON, args[1:])
	case "whoami":
		return runCommand(env, asJSON, args[1:], 0, whoami)
	case "project":
		if len(args) < 2 || args[1] != "create" {
			return usageError(env)
		}
		return runCommand(env, asJSON, args[2:], 1, createProject)
	case "spec":
		return runSpec(env, asJSON, args[1:])
	default:
		return usageError(env)
	}
}

// splitJSONFlag removes the global --json flag from args wherever it appears.
func splitJSONFlag(args []string) ([]string, bool) {
	rest := make([]string, 0, len(args))
	asJSON := false
	for _, a := range args {
		if a == "--json" {
			asJSON = true
			continue
		}
		rest = append(rest, a)
	}
	return rest, asJSON
}

func usageError(env Env) int {
	fmt.Fprintln(env.Stderr, usage)
	return 2
}
