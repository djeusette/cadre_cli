// Package cli dispatches cadre's commands.
package cli

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

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
	Stdin          io.Reader              // where a piped token comes from; the process's stdin when nil
	Terminal       func() bool            // whether stdin is a terminal; the process's stdin mode when nil
	ReadSecret     func() (string, error) // one line typed on the terminal with echo off; the terminal device when nil
	ReadPath       func() (string, error) // one line typed on the terminal, echoed, a file's path; the terminal device when nil
}

// Run executes the command in args and returns the exit code: 0 ok, 1 failure, 2 usage error.
func Run(args []string, env Env) int {
	args, asJSON := splitBoolFlag(args, "--json")
	args, wantsHelp := helpRequest(args)
	if wantsHelp || len(args) == 0 {
		return showHelp(env, args)
	}

	switch args[0] {
	case "version":
		fmt.Fprintf(env.Stdout, "cadre %s\n", env.Version)
		return 0
	case "login":
		return runLogin(env, asJSON, args[1:])
	case "whoami":
		return runCommand(env, asJSON, "whoami", args[1:], 0, whoami)
	case "project":
		return runProject(env, asJSON, args[1:])
	case "spec":
		return runSpec(env, asJSON, args[1:])
	case "pr":
		return runPr(env, asJSON, args[1:])
	case "repo":
		return runRepo(env, asJSON, args[1:])
	case "fly":
		return runFly(env, asJSON, args[1:])
	case "vercel":
		return runVercel(env, asJSON, args[1:])
	case "app-store":
		return runAppStore(env, asJSON, args[1:])
	case "google-play":
		return runGooglePlay(env, asJSON, args[1:])
	case "deploy":
		return runDeploy(env, asJSON, args[1:])
	case "upgrade":
		return runUpgrade(env, asJSON, args[1:])
	default:
		return unknownCommand(env, "", args[0])
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
