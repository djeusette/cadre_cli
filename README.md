# cadre CLI

`cadre` is the command-line client for [Cadre](https://colette-cadre.fly.dev), which shows where every milestone stands, on every repo, in deploy order. People and Claude Code (through the `cadre` skill) use it to sign in and register projects.

## Install

macOS (arm64, x86_64) and Linux (x86_64, aarch64):

```bash
mkdir -p ~/.local/bin
gh release download -R djeusette/cadre_cli --pattern "cadre_$(uname -s)_$(uname -m).tar.gz" -O - | tar -xz -C ~/.local/bin cadre
```

Make sure `~/.local/bin` is on your `PATH`, then run `cadre version`.

## Quick start

```bash
cadre login                       # approve the code in your browser
cadre whoami
cadre project create project.json
```

## Commands

| Command | What it does |
| --- | --- |
| `cadre login` | Signs in from the browser and stores a personal API key |
| `cadre whoami` | Shows the server and the account you're signed in as |
| `cadre project create <file>` | Registers a project, its milestones and their deploy order |
| `cadre version` | Prints the version |

Add `--json` to any command for machine-readable output.

## Configuration

The key and server URL live in `~/.config/cadre/config.json` (mode 0600). `CADRE_URL` overrides the server, which defaults to `https://colette-cadre.fly.dev`.

## Development

```bash
gofmt -l . && go vet ./... && go test ./...
```

Releases are built by GoReleaser when a `v*` tag is pushed.
