package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/djeusette/cadre_cli/internal/api"
)

// runRepo dispatches `repo set <repo> (--fly-app <app> [--fly-config <file>] [--status-url <url>]|--vercel-project <id>
// --name <app> --vercel-dir <folder>|--platform <name>)`, `repo unset <repo> --platform <name> [--name <app>]` and
// `repo show <repo>`.
func runRepo(env Env, asJSON bool, args []string) int {
	if len(args) == 0 {
		return showHelp(env, []string{"repo"})
	}
	rest := args[1:]
	switch args[0] {
	case "set":
		return runRepoSet(env, asJSON, rest)
	case "unset":
		platform, rest, ok := takeFlag(rest, "--platform")
		if !ok || platform == "" {
			return usageError(env, "repo unset")
		}
		name, rest, ok := takeFlag(rest, "--name")
		if !ok {
			return usageError(env, "repo unset")
		}
		return runCommand(env, asJSON, "repo unset", rest, 1, func(ctx context.Context, s *session, a []string) int {
			return unsetTarget(ctx, s, a[0], platform, name)
		})
	case "show":
		return runCommand(env, asJSON, "repo show", rest, 1, func(ctx context.Context, s *session, a []string) int {
			return showRepository(ctx, s, a[0])
		})
	default:
		return unknownCommand(env, "repo", args[0])
	}
}

// repoSetFlags are repo set's flags. Exactly one of flyApp, vercelProject and platform is given.
type repoSetFlags struct {
	flyApp, flyConfig, statusURL, platform string
	vercelProject, name, vercelDir         string
}

// takeRepoSetFlags reads repo set's flags out of args, leaving the rest, and reports whether they make one of the
// three forms: each flag belongs to one form only.
func takeRepoSetFlags(args []string) (repoSetFlags, []string, bool) {
	var f repoSetFlags
	for _, flag := range []struct {
		name string
		into *string
	}{
		{"--fly-app", &f.flyApp}, {"--fly-config", &f.flyConfig}, {"--status-url", &f.statusURL}, {"--platform", &f.platform},
		{"--vercel-project", &f.vercelProject}, {"--name", &f.name}, {"--vercel-dir", &f.vercelDir},
	} {
		var ok bool
		if *flag.into, args, ok = takeFlag(args, flag.name); !ok {
			return f, nil, false
		}
	}
	forms := 0
	for _, v := range []string{f.flyApp, f.vercelProject, f.platform} {
		if v != "" {
			forms++
		}
	}
	valid := forms == 1 &&
		(f.flyApp != "" || (f.flyConfig == "" && f.statusURL == "")) &&
		(f.vercelProject != "" || (f.name == "" && f.vercelDir == "")) &&
		(f.vercelProject == "" || (f.name != "" && f.vercelDir != "" && f.statusURL == "" && f.flyConfig == ""))
	return f, args, valid
}

func runRepoSet(env Env, asJSON bool, args []string) int {
	f, rest, ok := takeRepoSetFlags(args)
	if !ok {
		return usageError(env, "repo set")
	}
	platform, settings := f.platform, map[string]string{}
	switch {
	case f.flyApp != "":
		platform, settings["app"] = "fly", f.flyApp
		if f.flyConfig != "" {
			settings["config"] = f.flyConfig
		}
	case f.vercelProject != "":
		platform, settings["project"], settings["dir"] = "vercel", f.vercelProject, f.vercelDir
	}
	return runCommand(env, asJSON, "repo set", rest, 1, func(ctx context.Context, s *session, a []string) int {
		switch {
		case f.platform != "" && strings.EqualFold(platform, "fly"):
			fmt.Fprintf(s.env.Stderr, "Fly has its own flag: cadre repo set %s --fly-app <app>.\n", a[0])
			return 1
		case f.platform != "" && strings.EqualFold(platform, "vercel"):
			fmt.Fprintf(s.env.Stderr, "Vercel has its own flags: cadre repo set %s --vercel-project <id> --name <app> --vercel-dir <folder>.\n", a[0])
			return 1
		}
		return setTarget(ctx, s, a[0], platform, f.name, settings, f.statusURL)
	})
}

func setTarget(ctx context.Context, s *session, repo, platform, name string, settings map[string]string, statusURL string) int {
	if !s.requireKey() {
		return 1
	}

	target, err := s.client.SetTarget(ctx, repo, platform, name, settings, statusURL)
	if err != nil {
		return s.targetFailure(err, repo, platform, name)
	}

	switch {
	case s.asJSON:
		fmt.Fprint(s.env.Stdout, string(target.Raw))
	case target.Tracked:
		fmt.Fprintln(s.env.Stdout, connectedLine(target))
	default:
		fmt.Fprintf(s.env.Stdout, "cadre can't read %s deploys yet, so %s shows as not tracked.\n", displayName(target.Platform), target.Repo)
	}
	return 0
}

// connectedLine is what set says about a tracked target: what it connected to, the account reading it if any, and
// what it deploys with. It reads the server's answer, so a setting that didn't stick shows.
func connectedLine(t api.Target) string {
	what := "app " + fmt.Sprint(t.Settings["app"])
	if t.Platform == "vercel" {
		what = "project " + fmt.Sprint(t.Settings["project"]) + " as " + t.Name
	}
	line := "Connected " + t.Repo + " to the " + displayName(t.Platform) + " " + what
	if t.Account != nil {
		line += ", in the " + *t.Account + " " + accountKind(t.Platform)
	}
	return line + "." + deploysWith(t)
}

// deploysWith is the sentence saying what a target deploys with: a Fly target's config file, a Vercel target's
// folder. Other platforms have nothing to say.
func deploysWith(target api.Target) string {
	switch target.Platform {
	case "fly":
		if config, _ := target.Settings["config"].(string); config != "" {
			return " It deploys with " + config + "."
		}
		return " It deploys with fly.production.toml, else fly.toml."
	case "vercel":
		return " It deploys from " + fmt.Sprint(target.Settings["dir"]) + "."
	default:
		return ""
	}
}

// accountKind is what a platform calls the account that reads its apps: a team on Vercel, an org elsewhere.
func accountKind(platform string) string {
	if platform == "vercel" {
		return "team"
	}
	return "org"
}

func unsetTarget(ctx context.Context, s *session, repo, platform, name string) int {
	if !s.requireKey() {
		return 1
	}

	removed, err := s.client.UnsetTarget(ctx, repo, platform, name)
	if err != nil {
		return s.targetFailure(err, repo, platform, name)
	}

	switch {
	case s.asJSON:
		fmt.Fprint(s.env.Stdout, string(removed.Raw))
	case name != "":
		fmt.Fprintf(s.env.Stdout, "Removed %s's %s target %s.\n", removed.Repo, displayName(removed.Platform), name)
	default:
		fmt.Fprintf(s.env.Stdout, "Removed %s's %s target.\n", removed.Repo, displayName(removed.Platform))
	}
	return 0
}

func showRepository(ctx context.Context, s *session, repo string) int {
	if !s.requireKey() {
		return 1
	}

	shown, err := s.client.ShowRepository(ctx, repo)
	if err != nil {
		return s.repoFailure(err, repo, "")
	}

	switch {
	case s.asJSON:
		fmt.Fprint(s.env.Stdout, string(shown.Raw))
	case len(shown.Targets) == 0:
		fmt.Fprintf(s.env.Stdout, "%s (%s) has no deploy target yet.\n", shown.Name, shown.GitHubFullName)
	default:
		fmt.Fprint(s.env.Stdout, repositoryText(shown))
	}
	return 0
}

// repositoryText is show's answer: the repo, then a line per target with the platform column padded to the longest
// name plus two spaces.
func repositoryText(repo api.Repository) string {
	width := 0
	for _, t := range repo.Targets {
		width = max(width, len(displayName(t.Platform)))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s)\n", repo.Name, repo.GitHubFullName)
	for _, t := range repo.Targets {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, displayName(t.Platform), targetState(t))
		if line := statusLine(t); line != "" {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}
	return b.String()
}

// targetState is what show says about a target: its name if it has one, its settings as `key value` pairs sorted by
// key, the org (team, on Vercel) reading it and its last deploy; or that it is not tracked.
func targetState(t api.Target) string {
	if !t.Tracked {
		return "not tracked"
	}
	pairs := make([]string, 0, len(t.Settings)+3)
	if t.Name != "" {
		pairs = append(pairs, t.Name)
	}
	for _, key := range slices.Sorted(maps.Keys(t.Settings)) {
		pairs = append(pairs, key+" "+fmt.Sprint(t.Settings[key]))
	}
	if t.Account != nil {
		pairs = append(pairs, accountKind(t.Platform)+" "+*t.Account)
	}
	pairs = append(pairs, lastDeployText(t.LastDeploy))
	return strings.Join(pairs, ", ")
}

// statusLine is what show says about a target's live state, or empty when there is nothing to say. A target with a
// status URL always gets one; one without gets a line only when the server still reports a live state, as a Fly app
// can through its health checks.
func statusLine(t api.Target) string {
	state := liveText(t)
	switch {
	case t.StatusURL != "" && state == "":
		return "Status URL " + oneLine(t.StatusURL) + ": not checked yet"
	case t.StatusURL != "":
		return "Status URL " + oneLine(t.StatusURL) + ": " + state
	case state != "":
		return "Status: " + state
	default:
		return ""
	}
}

// liveText reads the target's live state, or is empty when it has none.
func liveText(t api.Target) string {
	switch t.Live {
	case "live":
		return "live"
	case "not_live":
		if t.NotLiveSince == nil {
			return "not live"
		}
		return "not live since " + t.NotLiveSince.UTC().Format("Jan 2, 15:04")
	case "not_checked":
		return "not checked yet"
	default:
		return ""
	}
}

// lastDeployText says when a target last deployed, in UTC, and the commit's first 7 characters.
func lastDeployText(d *api.LastDeploy) string {
	switch {
	case d == nil:
		return "no deploy seen yet"
	case d.Commit == nil:
		return "last deploy " + d.DeployedAt.UTC().Format("Jan 2") + " (commit unknown)"
	default:
		return "last deploy " + d.DeployedAt.UTC().Format("Jan 2") + " (" + (*d.Commit)[:min(7, len(*d.Commit))] + ")"
	}
}

// repoFailure prints a failed repo call to stderr and returns exit code 1. platform is the one the person typed,
// or empty when the call names none.
func (s *session) repoFailure(err error, repo, platform string) int {
	return s.targetFailure(err, repo, platform, "")
}

// targetFailure is repoFailure for a call that names one of the platform's targets: target is its name, or empty.
func (s *session) targetFailure(err error, repo, platform, target string) int {
	lower := strings.ToLower(platform)
	name := displayName(lower)
	switch {
	case errors.Is(err, api.ErrRepositoryNotFound):
		fmt.Fprintf(s.env.Stderr, "No repo %s on %s.\n", repo, s.baseURL)
	case errors.Is(err, api.ErrTargetNotFound):
		fmt.Fprintf(s.env.Stderr, "%s has no %s target%s.\n", repo, name, strings.TrimRight(" "+target, " "))
	case errors.Is(err, api.ErrPlatformNotConnected):
		fmt.Fprintf(s.env.Stderr, "%s isn't connected on %s yet. %s\n", name, s.baseURL, addAccountHint(lower))
	case errors.Is(err, api.ErrPlatformUnreachable):
		fmt.Fprintf(s.env.Stderr, "Cadre couldn't reach %s. Try again in a minute.\n", name)
	default:
		return s.failAPI(err)
	}
	return 1
}

// addAccountHint is how to connect the platform's first account.
func addAccountHint(platform string) string {
	if platform == "vercel" {
		return "Add a team with cadre vercel team add <team>."
	}
	return "Add an org with cadre fly org add <org>."
}

// displayName is a platform's name with its first letter upper-cased: fly is Fly.
func displayName(platform string) string {
	if platform == "" {
		return ""
	}
	return strings.ToUpper(platform[:1]) + platform[1:]
}
