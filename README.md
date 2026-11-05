# cadre CLI

`cadre` is the command-line client for [Cadre](https://colette-cadre.fly.dev), which shows where every milestone stands, on every repo, in deploy order. People and Claude Code (through the `cadre` skill) use it for signing in, registering projects, pulling and pushing milestone specs, connecting Fly orgs, saying which platform each repo deploys to, and deploying.

## Install

macOS (arm64, x86_64) and Linux (x86_64, aarch64):

```bash
mkdir -p ~/.local/bin
gh release download -R djeusette/cadre_cli --pattern "cadre_$(uname -s)_$(uname -m).tar.gz" -O - | tar -xz -C ~/.local/bin cadre
```

Make sure `~/.local/bin` is on your `PATH`, then run `cadre version`. From 0.5.0 on, `cadre upgrade` updates it.

## Quick start

```bash
cadre login --url https://colette-cadre.fly.dev   # your team's Cadre; approve the code in your browser
cadre whoami
cadre project create project.json
cadre spec pull wish-reminders/2 -o spec.md --json   # note the "version"
cadre spec push wish-reminders/2 spec.md --base <version>
```

## Commands

`cadre help` lists every command, and `cadre help <command>` or `cadre <command> --help` gives its arguments, flags and examples.

| Command | What it does |
| --- | --- |
| `cadre login --url <url>` | Signs in to the Cadre at `<url>` from the browser, and stores that URL and a personal API key. The URL is required |
| `cadre whoami` | Shows the server and the account you're signed in as |
| `cadre project create <file>` | Registers a project, its milestones and their deploy order, with each milestone's spec and design link and the project's design canvas link when the file has them |
| `cadre project pull <slug> [-o <file>]` | Prints the project as the file `project update` takes (its `version` and each milestone's `number` included), or writes it to `<file>`. With `--json`, prints the answer as received |
| `cadre project update <file>` | Sends a pulled and edited file, unchanged, to the project named by its `slug`, and prints the new version and the project's page. Refused when someone updated the project since you pulled it, or when it is archived |
| `cadre project archive <slug>` | Archives a project: it leaves the Overview until it is unarchived |
| `cadre project unarchive <slug>` | Brings an archived project back and prints its page |
| `cadre spec pull <project>/<n> [-o <file>]` | Prints the newest version of milestone `<n>`'s spec exactly as stored, or writes it to `<file>`. With `--json`, prints the answer, whose `version` names that version |
| `cadre spec push <project>/<n> <file> [--base <version>]` | Uploads a markdown file, unchanged, as the newest version of milestone `<n>`'s spec, and prints the project's page. With `--base`, the push is refused when someone pushed a newer version since that one: pull again |
| `cadre pr link <pr-url> <project>/<n>` | Links a pull request to a milestone; `--none` unlinks it |
| `cadre repo set <repo> (--fly-app <app> [--fly-config <file>]\|--platform <name>)` | Sets where a repo deploys: `--fly-app` connects it to a Fly app, which cadre then tracks, and `--fly-config` names the Fly config file it deploys with, for the whole team; `--platform` names any other platform, which shows as not tracked. Setting a platform again replaces its settings, so pass both flags |
| `cadre repo unset <repo> --platform <name>` | Removes a repo's deploy target on that platform |
| `cadre repo show <repo>` | Lists a repo's deploy targets, the Fly org each is read through, whether cadre can track each, and each tracked target's last deploy (date in UTC and commit) |
| `cadre fly org add <org>` | Connects a Fly org with a read-only token (`fly tokens create readonly -o <org> -n cadre \| cadre fly org add <org>`), piped in or typed when asked with echo off. Adding it again replaces the token |
| `cadre fly org remove <org>` | Disconnects a Fly org, unless a repo's target still reads it |
| `cadre deploy <repo> [--yes]` | Runs `fly deploy` at the top level of a checkout of the repo, with the commit as the image label. Refused unless the checkout is on `main`, clean, and equal to `origin/main` after a fetch, and the repo has a Fly app. The config file is the one set with `--fly-config`, else `fly.production.toml` if the checkout has one, else `fly.toml`, and it is refused if that file isn't in the checkout. Open Before deploying items are listed and asked about, and with nothing to read the answer is no; `--yes` goes on without asking. Right before `fly` runs, the branch, the commit and the working tree are checked again, so a change made in the meantime stops the deploy. Needs `fly` installed and signed in |
| `cadre upgrade` | Replaces this `cadre` with the latest release on GitHub once the download matches the release's checksums, and prints both versions. It changes nothing when this is already the latest, and refuses a development build. Needs no sign-in |
| `cadre version` | Prints the version |
| `cadre help [<command>]` | Prints help for a command or a group such as `project`, or every command with none. Needs no sign-in |

Add `--json` to any command for machine-readable output.

## Configuration

`cadre login --url <url>` stores the server URL and the key in `~/.config/cadre/config.json` (mode 0600). Every other command talks to that stored URL and nothing else: there is no default server and no environment override. Before the first login, commands answer `Not signed in. Run cadre login --url <url>.`

## Development

```bash
gofmt -l . && go vet ./... && go test ./...
```

Releases are built by GoReleaser when a `v*` tag is pushed.
