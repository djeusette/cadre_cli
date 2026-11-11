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

// runRepo dispatches `repo set <repo> (--fly-app <app> [--name <app>] [--fly-config <file>]
// [--status-url <url>|--no-status-url]|--vercel-project <id> --name <app> --vercel-dir <folder>
// [--status-url <url>|--no-status-url]|--platform <name>|[--app-store-app <id>] [--play-package <name>] [--name <app>])`,
// `repo unset <repo> (--name <app>|--platform <name> [--name <app>])` and `repo show <repo>`.
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
		if !ok {
			return usageError(env, "repo unset")
		}
		name, rest, ok := takeFlag(rest, "--name")
		if !ok || (platform == "" && name == "") {
			return usageError(env, "repo unset")
		}
		return runCommand(env, asJSON, "repo unset", rest, 1, func(ctx context.Context, s *session, a []string) int {
			if platform == "" {
				return unsetApp(ctx, s, a[0], name)
			}
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
	appStoreApp, playPackage               string
	noStatusURL                            bool
}

// takeRepoSetFlags reads repo set's flags out of args, leaving the rest, and reports whether they make one of the
// three forms: each flag belongs to one form only, except that --status-url and --no-status-url (one of them at most)
// belong to either app form.
func takeRepoSetFlags(args []string) (repoSetFlags, []string, bool) {
	var f repoSetFlags
	for _, flag := range []struct {
		name string
		into *string
	}{
		{"--fly-app", &f.flyApp}, {"--fly-config", &f.flyConfig}, {"--status-url", &f.statusURL}, {"--platform", &f.platform},
		{"--vercel-project", &f.vercelProject}, {"--name", &f.name}, {"--vercel-dir", &f.vercelDir},
		{"--app-store-app", &f.appStoreApp}, {"--play-package", &f.playPackage},
	} {
		var ok bool
		if *flag.into, args, ok = takeFlag(args, flag.name); !ok {
			return f, nil, false
		}
	}
	args, f.noStatusURL = splitBoolFlag(args, "--no-status-url")
	forms := 0
	for _, v := range []string{f.flyApp, f.vercelProject, f.platform, f.mobileForm()} {
		if v != "" {
			forms++
		}
	}
	setsStatusURL := f.statusURL != "" || f.noStatusURL
	valid := forms == 1 &&
		(f.flyApp != "" || f.flyConfig == "") &&
		(f.vercelProject != "" || f.flyApp != "" || !setsStatusURL) &&
		(f.statusURL == "" || !f.noStatusURL) &&
		(f.vercelProject != "" || f.vercelDir == "") &&
		(f.vercelProject != "" || f.flyApp != "" || f.mobileForm() != "" || f.name == "") &&
		(f.vercelProject == "" || (f.name != "" && f.vercelDir != "" && f.flyConfig == ""))
	return f, args, valid
}

// mobileForm is "mobile" when a store's flag is given, else empty.
func (f repoSetFlags) mobileForm() string {
	if f.appStoreApp != "" || f.playPackage != "" {
		return "mobile"
	}
	return ""
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
	case f.mobileForm() != "":
		platform = "mobile"
		if f.appStoreApp != "" {
			settings["app_store_app"] = f.appStoreApp
		}
		if f.playPackage != "" {
			settings["play_package"] = f.playPackage
		}
	}
	return runCommand(env, asJSON, "repo set", rest, 1, func(ctx context.Context, s *session, a []string) int {
		switch {
		case f.platform != "" && strings.EqualFold(platform, "fly"):
			fmt.Fprintf(s.env.Stderr, "Fly has its own flag: cadre repo set %s --fly-app <app>.\n", a[0])
			return 1
		case f.platform != "" && strings.EqualFold(platform, "vercel"):
			fmt.Fprintf(s.env.Stderr, "Vercel has its own flags: cadre repo set %s --vercel-project <id> --name <app> --vercel-dir <folder>.\n", a[0])
			return 1
		case f.platform != "" && strings.EqualFold(platform, "mobile"):
			fmt.Fprintf(s.env.Stderr, "A mobile app has its own flags: cadre repo set %s --app-store-app <id> --play-package <name>.\n", a[0])
			return 1
		}
		return setTarget(ctx, s, a[0], platform, f.name, settings, api.StatusURL{URL: f.statusURL, None: f.noStatusURL})
	})
}

func setTarget(ctx context.Context, s *session, repo, platform, name string, settings map[string]string, statusURL api.StatusURL) int {
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
	case target.Platform == "mobile":
		fmt.Fprintf(s.env.Stdout, "Connected %s to its stores%s.\nCadre asks each store which build is public every 15 minutes.\n",
			target.Repo, storeListSuffix(target.Stores))
	case target.Tracked:
		fmt.Fprintln(s.env.Stdout, connectedLine(target))
		if platform == "fly" && name == "" {
			s.sayFurtherApp(ctx, repo)
		}
		if line := statusKeptLine(target); line != "" {
			fmt.Fprintln(s.env.Stdout, line)
		}
	default:
		fmt.Fprintf(s.env.Stdout, "cadre can't read %s deploys yet, so %s shows as not tracked.\n", displayName(target.Platform), target.Repo)
	}
	return 0
}

// storeList is a mobile app's stores, each with the account reading it, as the line after a set names them.
// storeListSuffix is ": " and the stores, or nothing when the answer lists none.
func storeListSuffix(stores []api.TargetStore) string {
	if len(stores) == 0 {
		return ""
	}
	return ": " + storeList(stores)
}

func storeList(stores []api.TargetStore) string {
	items := make([]string, len(stores))
	for i, st := range stores {
		items[i] = oneLine(st.Label) + " (" + storeAccount(st) + ")"
	}
	return strings.Join(items, ", ")
}

// storeAccount is the account reading a store: an App Store key says so, a Google Play account is its email alone.
func storeAccount(st api.TargetStore) string {
	if st.Store == "app_store" {
		return "key " + oneLine(st.Account)
	}
	return oneLine(st.Account)
}

// statusKeptLine is what set says about the status URL the server kept, or why it kept none; empty when there is
// nothing to say.
func statusKeptLine(t api.Target) string {
	switch {
	case t.StatusURL != "":
		return "Status URL: " + oneLine(t.StatusURL)
	case t.StatusURLNote != "":
		return "No status URL: " + oneLine(t.StatusURLNote) + "."
	default:
		return ""
	}
}

// sayFurtherApp says that the repo now has several apps, and how to move one instead of adding another. Two Fly apps
// trigger it, but it counts and lists every app of the repo. The set already happened, so a failed read of the repo
// says nothing rather than failing it.
func (s *session) sayFurtherApp(ctx context.Context, repo string) {
	shown, err := s.client.ShowRepository(ctx, repo)
	if err != nil {
		return
	}
	flyApps := slices.DeleteFunc(slices.Clone(shown.Targets), func(t api.Target) bool { return t.Platform != "fly" })
	if len(flyApps) < 2 {
		return
	}
	apps := slices.Clone(shown.Targets)
	slices.SortStableFunc(apps, byName)
	fmt.Fprintf(s.env.Stdout, "%s now has %d apps: %s. To move an app instead, pass --name <app>.\n", repo, len(apps), targetNames(apps))
}

// connectedLine is what set says about a tracked target: what it connected to, under what name when the app's name
// isn't its Fly app's or it is on Vercel, the account reading it if any, and what it deploys with. It reads the
// server's answer, so a setting that didn't stick shows.
func connectedLine(t api.Target) string {
	what := "app" + spaced(setting(t, "app"))
	if t.Platform == "fly" && t.Name != "" && t.Name != setting(t, "app") {
		what += " as" + spaced(oneLine(t.Name))
	}
	if t.Platform == "vercel" {
		what = "project" + spaced(setting(t, "project")) + " as" + spaced(oneLine(t.Name))
	}
	line := "Connected " + t.Repo + " to the " + displayName(t.Platform) + " " + what
	if t.Account != nil {
		line += ", in the " + oneLine(*t.Account) + " " + accountKind(t.Platform)
	}
	return line + "." + deploysWith(t)
}

// setting is the target's setting key as one line of text, or empty when it has none.
func setting(t api.Target, key string) string {
	v, ok := t.Settings[key]
	if !ok || v == nil {
		return ""
	}
	return oneLine(fmt.Sprint(v))
}

// spaced is text with a space before it, or nothing when text is empty.
func spaced(text string) string {
	if text == "" {
		return ""
	}
	return " " + text
}

// deploysWith is the sentence saying what a target deploys with: a Fly target's config file, a Vercel target's
// folder. Other platforms, and a Vercel target without a folder, have nothing to say.
func deploysWith(target api.Target) string {
	switch target.Platform {
	case "fly":
		if config := setting(target, "config"); config != "" {
			return " It deploys with " + config + "."
		}
		return " It deploys with fly.production.toml, else fly.toml."
	case "vercel":
		return deploysFrom(setting(target, "dir"))
	}
	return ""
}

// deploysFrom is the sentence saying which folder a Vercel app deploys from, or empty when the target names none.
func deploysFrom(dir string) string {
	switch dir {
	case "":
		return ""
	case ".":
		return " It deploys from the repo's root."
	default:
		return " It deploys from " + dir + "."
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
	return s.reportRemoved(removed, name)
}

// unsetApp removes the repo's app called name, whatever its platform.
func unsetApp(ctx context.Context, s *session, repo, name string) int {
	if !s.requireKey() {
		return 1
	}

	removed, err := s.client.UnsetApp(ctx, repo, name)
	switch {
	case errors.Is(err, api.ErrTargetNotFound):
		fmt.Fprintf(s.env.Stderr, "%s has no app %s.\n", repo, name)
		return 1
	case err != nil:
		return s.repoFailure(err, repo, "")
	}
	return s.reportRemoved(removed, name)
}

// reportRemoved prints what an unset removed: the server's answer with --json, else a line naming the app if given.
func (s *session) reportRemoved(removed api.Unset, name string) int {
	switch {
	case s.asJSON:
		fmt.Fprint(s.env.Stdout, string(removed.Raw))
	case name != "":
		fmt.Fprintf(s.env.Stdout, "Removed %s's %s app %s.\n", removed.Repo, displayName(removed.Platform), name)
	default:
		fmt.Fprintf(s.env.Stdout, "Removed %s's %s app.\n", removed.Repo, displayName(removed.Platform))
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
		b.WriteString(storeLines(t.Stores))
	}
	return b.String()
}

// storeLines is a line under a mobile target for each of its stores: the label padded to the longest, with Google
// Play's length as the least, the build that is public there and the account reading it.
func storeLines(stores []api.TargetStore) string {
	width := len("Google Play")
	for _, st := range stores {
		width = max(width, len(oneLine(st.Label)))
	}
	var b strings.Builder
	for _, st := range stores {
		fmt.Fprintf(&b, "    %-*s   %s · %s\n", width, oneLine(st.Label), publicBuildText(st.PublicBuild), storeAccount(st))
	}
	return b.String()
}

// publicBuildText says which build is public in a store and since when, in UTC; a build with no version shows its
// number alone.
func publicBuildText(b *api.StoreBuild) string {
	switch {
	case b == nil:
		return "no public build seen yet"
	case b.Version == nil:
		return fmt.Sprintf("%d public since %s", b.BuildNumber, b.Since.UTC().Format("Jan 2, 15:04"))
	default:
		return fmt.Sprintf("%s (%d) public since %s", oneLine(*b.Version), b.BuildNumber, b.Since.UTC().Format("Jan 2, 15:04"))
	}
}

// targetState is what show says about a target: its name if it has one, its settings as `key value` pairs sorted by
// key, the org (team, on Vercel) reading it and its last deploy; or that it is not tracked.
func targetState(t api.Target) string {
	if t.Platform == "mobile" {
		return mobileState(t)
	}
	if !t.Tracked {
		return "not tracked"
	}
	pairs := make([]string, 0, len(t.Settings)+3)
	if t.Name != "" && t.Name != setting(t, "app") {
		pairs = append(pairs, oneLine(t.Name))
	}
	for _, key := range slices.Sorted(maps.Keys(t.Settings)) {
		pairs = append(pairs, oneLine(key)+" "+setting(t, key))
	}
	if t.Account != nil {
		pairs = append(pairs, accountKind(t.Platform)+" "+oneLine(*t.Account))
	}
	pairs = append(pairs, lastDeployText(t.LastDeploy))
	return strings.Join(pairs, ", ")
}

// mobileState is what show says on a mobile target's own line: its name, then its settings sorted by key. Whether the
// stores see a build is the lines under it; "not tracked" would hide that, and the commit a build was made from is
// not known yet.
func mobileState(t api.Target) string {
	var pairs []string
	if t.Name != "" {
		pairs = append(pairs, oneLine(t.Name))
	}
	for _, key := range slices.Sorted(maps.Keys(t.Settings)) {
		pairs = append(pairs, oneLine(key)+" "+setting(t, key))
	}
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
	switch platform {
	case "":
		return ""
	case "app_store":
		return "App Store"
	case "google_play":
		return "Google Play"
	}
	return strings.ToUpper(platform[:1]) + platform[1:]
}
