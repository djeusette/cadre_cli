package main

import (
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/djeusette/cadre_cli/internal/cli"
	"github.com/djeusette/cadre_cli/internal/release"
)

// version is set by GoReleaser: -X main.version={{.Version}}.
var version = "dev"

func main() {
	env := cli.Env{
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		Getenv:      os.Getenv,
		OpenBrowser: openBrowser,
		Sleep:       time.Sleep,
		GitHubURL:   release.GitHubURL,
		Executable:  os.Executable,
		Version:     version,
	}
	os.Exit(cli.Run(os.Args[1:], env))
}

func openBrowser(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, url).Start()
}
