// Package cli dispatches cadre's commands.
package cli

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

const usage = "Usage: cadre <login --url <url>|whoami|project create <file>|project pull <slug> [-o <file>]|project update <file>|project archive <slug>|project unarchive <slug>|spec pull <project>/<n> [-o <file>]|spec push <project>/<n> <file> [--base <version>]|pr link <pr-url> (<project>/<n>|--none)|upgrade|version> [--json]"

// Env carries everything a command touches outside its arguments, so tests can fake it.
type Env struct {
	Stdout, Stderr io.Writer
	Getenv         func(string) string
	OpenBrowser    func(string) error
	Sleep          func(time.Duration)
	HTTP           *http.Client
	GitHubURL      string                 // GitHub's API, where upgrade finds the latest release
	Executable     func() (string, error) // the running binary, which upgrade replaces
	Version        string
}

// Run executes the command in args and returns the exit code: 0 ok, 1 failure, 2 usage error.
func Run(args []string, env Env) int {
	args, asJSON := splitBoolFlag(args, "--json")
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
		return runProject(env, asJSON, args[1:])
	case "spec":
		return runSpec(env, asJSON, args[1:])
	case "pr":
		return runPr(env, asJSON, args[1:])
	case "upgrade":
		return runUpgrade(env, asJSON, args[1:])
	default:
		return usageError(env)
	}
}

// splitBoolFlag removes the boolean flag name, such as the global --json, from args wherever it appears,
// and reports whether it was there.
func splitBoolFlag(args []string, name string) ([]string, bool) {
	rest := make([]string, 0, len(args))
	found := false
	for _, a := range args {
		if a == name {
			found = true
			continue
		}
		rest = append(rest, a)
	}
	return rest, found
}

func usageError(env Env) int {
	fmt.Fprintln(env.Stderr, usage)
	return 2
}
