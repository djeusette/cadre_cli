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

// runRepo dispatches `repo set <repo> (--fly-app <app>|--platform <name>)`, `repo unset <repo> --platform <name>`
// and `repo show <repo>`; any other form is a usage error.
func runRepo(env Env, asJSON bool, args []string) int {
	if len(args) == 0 {
		return usageError(env)
	}
	rest := args[1:]
	switch args[0] {
	case "set":
		flyApp, rest, ok := takeFlag(rest, "--fly-app")
		if !ok {
			return usageError(env)
		}
		platform, rest, ok := takeFlag(rest, "--platform")
		if !ok || (flyApp == "") == (platform == "") {
			return usageError(env)
		}
		settings := map[string]string{}
		if flyApp != "" {
			platform, settings["app"] = "fly", flyApp
		}
		return runCommand(env, asJSON, rest, 1, func(ctx context.Context, s *session, a []string) int {
			if flyApp == "" && strings.EqualFold(platform, "fly") {
				fmt.Fprintf(s.env.Stderr, "Fly has its own flag: cadre repo set %s --fly-app <app>.\n", a[0])
				return 1
			}
			return setTarget(ctx, s, a[0], platform, settings)
		})
	case "unset":
		platform, rest, ok := takeFlag(rest, "--platform")
		if !ok || platform == "" {
			return usageError(env)
		}
		return runCommand(env, asJSON, rest, 1, func(ctx context.Context, s *session, a []string) int {
			return unsetTarget(ctx, s, a[0], platform)
		})
	case "show":
		return runCommand(env, asJSON, rest, 1, func(ctx context.Context, s *session, a []string) int {
			return showRepository(ctx, s, a[0])
		})
	default:
		return usageError(env)
	}
}

func setTarget(ctx context.Context, s *session, repo, platform string, settings map[string]string) int {
	if !s.requireKey() {
		return 1
	}

	target, err := s.client.SetTarget(ctx, repo, platform, settings)
	if err != nil {
		return s.repoFailure(err, repo, platform)
	}

	switch {
	case s.asJSON:
		fmt.Fprint(s.env.Stdout, string(target.Raw))
	case target.Tracked && target.Account != nil:
		fmt.Fprintf(s.env.Stdout, "Connected %s to the %s app %s, in the %s org.\n", target.Repo, displayName(target.Platform), fmt.Sprint(target.Settings["app"]), *target.Account)
	case target.Tracked:
		fmt.Fprintf(s.env.Stdout, "Connected %s to the %s app %s.\n", target.Repo, displayName(target.Platform), fmt.Sprint(target.Settings["app"]))
	default:
		fmt.Fprintf(s.env.Stdout, "cadre can't read %s deploys yet, so %s shows as not tracked.\n", displayName(target.Platform), target.Repo)
	}
	return 0
}

func unsetTarget(ctx context.Context, s *session, repo, platform string) int {
	if !s.requireKey() {
		return 1
	}

	removed, err := s.client.UnsetTarget(ctx, repo, platform)
	if err != nil {
		return s.repoFailure(err, repo, platform)
	}

	if s.asJSON {
		fmt.Fprint(s.env.Stdout, string(removed.Raw))
		return 0
	}
	fmt.Fprintf(s.env.Stdout, "Removed %s's %s target.\n", removed.Repo, displayName(removed.Platform))
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
	}
	return b.String()
}

// targetState is what show says about a target: its settings as `key value` pairs sorted by key, or that it is not
// tracked.
func targetState(t api.Target) string {
	if !t.Tracked {
		return "not tracked"
	}
	pairs := make([]string, 0, len(t.Settings))
	for _, key := range slices.Sorted(maps.Keys(t.Settings)) {
		pairs = append(pairs, key+" "+fmt.Sprint(t.Settings[key]))
	}
	if t.Account != nil {
		pairs = append(pairs, "org "+*t.Account)
	}
	if len(pairs) == 0 {
		return "tracked"
	}
	return strings.Join(pairs, ", ")
}

// repoFailure prints a failed repo call to stderr and returns exit code 1. platform is the one the person typed,
// or empty when the call names none.
func (s *session) repoFailure(err error, repo, platform string) int {
	name := displayName(strings.ToLower(platform))
	switch {
	case errors.Is(err, api.ErrRepositoryNotFound):
		fmt.Fprintf(s.env.Stderr, "No repo %s on %s.\n", repo, s.baseURL)
	case errors.Is(err, api.ErrTargetNotFound):
		fmt.Fprintf(s.env.Stderr, "%s has no %s target.\n", repo, name)
	case errors.Is(err, api.ErrPlatformNotConnected):
		fmt.Fprintf(s.env.Stderr, "%s isn't connected on %s yet. Add an org with cadre fly org add <org>.\n", name, s.baseURL)
	case errors.Is(err, api.ErrPlatformUnreachable):
		fmt.Fprintf(s.env.Stderr, "Cadre couldn't reach %s. Try again in a minute.\n", name)
	default:
		return s.failAPI(err)
	}
	return 1
}

// displayName is a platform's name with its first letter upper-cased: fly is Fly.
func displayName(platform string) string {
	if platform == "" {
		return ""
	}
	return strings.ToUpper(platform[:1]) + platform[1:]
}
