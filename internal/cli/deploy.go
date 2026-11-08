package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unicode"

	"github.com/djeusette/cadre_cli/internal/api"
	"github.com/djeusette/cadre_cli/internal/git"
)

const msgNotDeployed = "Not deployed."

// The refusals that need nothing from the repo. Their text is printed as it is.
var (
	errNotOnMain      = errors.New("Not on main. Run git switch main.")
	errNotClean       = errors.New("Uncommitted changes or untracked files. Commit or stash them first.")
	errMainDiffers    = errors.New("main differs from origin/main. Run git pull, or push your commits, then deploy again.")
	errFlyNotFound    = errors.New("fly isn't installed. See https://fly.io/docs/flyctl/install/.")
	errFlyNotSignedIn = errors.New("fly isn't signed in. Run fly auth login.")
)

// deployment is what the checks learn, in order, and all that the run needs.
type deployment struct {
	repository api.Repository
	targets    []api.Target // the apps it deploys, all on one platform, in name order
	checkout   git.Checkout
	commit     string   // the full commit it deploys: for Fly, the one main and origin/main agree on
	flyApps    []flyApp // the targets, as the Fly checks find them
	fly        string   // the path to flyctl or fly
}

// flyApp is one Fly target, as the checks find it.
type flyApp struct {
	name      string // its name in cadre
	app       string // the Fly app's name
	stored    string // the config file cadre stores for it, or empty
	statusURL string // the target's, or empty
	config    string // the config file it deploys with
}

// deployFlags are deploy's flags. At most one of app and all is given.
type deployFlags struct {
	app      string // the one app to deploy, by its name in cadre
	all, yes bool
}

// deployChecks run in this order. The first to return an error ends the deploy, with that error as the refusal.
var deployChecks = []func(context.Context, *deployment) error{
	checkCheckout,
	checkBranch,
	checkClean,
	checkOrigin,
	findApps,
	findFly,
}

// runDeploy dispatches `deploy <repo> [--app <name>|--all] [--yes]`.
func runDeploy(env Env, asJSON bool, args []string) int {
	var flags deployFlags
	args, flags.yes = splitBoolFlag(args, "--yes")
	args, flags.all = splitBoolFlag(args, "--all")
	app, args, ok := takeFlag(args, "--app")
	if !ok || (app != "" && flags.all) {
		return usageError(env, "deploy")
	}
	flags.app = app
	return runCommand(env, asJSON, "deploy", args, 1, func(ctx context.Context, s *session, a []string) int {
		return deploy(ctx, s, a[0], flags)
	})
}

// deploy reads the repo once and picks the apps it deploys, then takes the Vercel path for Vercel apps and the Fly path
// for any other pick. Anything else ends here.
func deploy(ctx context.Context, s *session, repo string, flags deployFlags) int {
	if !s.requireKey() {
		return 1
	}
	shown, err := s.client.ShowRepository(ctx, repo)
	if err != nil {
		return s.repoFailure(err, repo, "")
	}
	targets, err := pickTargets(shown, flags)
	if err != nil {
		return fail(s.env, err)
	}
	d := deployment{repository: shown, targets: targets}
	if len(targets) > 0 && targets[0].Platform == "vercel" {
		return vercelDeploy(ctx, s, d, flags.yes)
	}
	return deployFlyApps(ctx, s, &d, flags.yes)
}

// pickTargets is the apps the deploy takes, from those cadre deploy can deploy. With none it takes none, so the Fly
// checks run first, then say how to connect one. With no flag, a repo with one app takes it, and a repo with several is
// refused, listing them. --app takes the app of that name. --all takes every app, on one platform only.
func pickTargets(repo api.Repository, flags deployFlags) ([]api.Target, error) {
	candidates := deployable(repo.Targets)
	switch {
	case len(candidates) == 0:
		return nil, nil
	case flags.app != "":
		i := slices.IndexFunc(candidates, func(t api.Target) bool { return appName(t) == flags.app })
		if i < 0 {
			return nil, fmt.Errorf("%s has no app %s. Its apps: %s.", repo.Name, flags.app, targetNames(candidates))
		}
		return candidates[i : i+1], nil
	case flags.all:
		if slices.ContainsFunc(candidates, func(t api.Target) bool { return t.Platform != candidates[0].Platform }) {
			return nil, fmt.Errorf("%s has apps on more than one platform. Deploy each with --app <name>, in deploy order.", repo.Name)
		}
		return candidates, nil
	case len(candidates) > 1:
		return nil, fmt.Errorf("%s has %d apps: %s. Pass --app <name> or --all.", repo.Name, len(candidates), targetNames(candidates))
	default:
		return candidates, nil
	}
}

// deployable is the targets cadre deploy can deploy, the tracked ones on Fly or Vercel, in name order.
func deployable(targets []api.Target) []api.Target {
	var found []api.Target
	for _, t := range targets {
		if t.Tracked && (t.Platform == "fly" || t.Platform == "vercel") {
			found = append(found, t)
		}
	}
	slices.SortStableFunc(found, func(a, b api.Target) int { return strings.Compare(appName(a), appName(b)) })
	return found
}

// appName is the target's name in cadre, else its app setting, for a cadre that doesn't name Fly targets yet.
func appName(t api.Target) string {
	if t.Name != "" {
		return t.Name
	}
	app, _ := t.Settings["app"].(string)
	return app
}

// targetNames is the targets' names, each on one line, joined by commas.
func targetNames(targets []api.Target) string {
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = oneLine(appName(t))
	}
	return strings.Join(names, ", ")
}

// deployFlyApps runs fly for each app in turn, once every check has passed for all of them and the open items are
// settled.
func deployFlyApps(ctx context.Context, s *session, d *deployment, yes bool) int {
	for _, check := range deployChecks {
		if err := check(ctx, d); err != nil {
			return fail(s.env, err)
		}
	}

	next, err := s.client.NextDeploy(ctx, d.repository.Name)
	if err != nil {
		return s.repoFailure(err, d.repository.Name, "")
	}
	if !s.settleOpenItems(next.BeforeDeploying, nil, yes) {
		fmt.Fprintln(s.env.Stderr, msgNotDeployed)
		return 1
	}
	return s.runFlyApps(ctx, d)
}

// runFlyApps deploys each app in turn, and repeats the local checks before each. cadre stays up through SIGINT and
// SIGTERM until the run ends, as dying first would leave a deploy running with nobody to report how it went, and a
// signal stops the run before the next app. The first app that is refused, fails or is stopped ends the run, so the
// apps after it stay as they were, and once an app was tried, the run names them. A failed fly ends it with fly's
// status.
func (s *session) runFlyApps(ctx context.Context, d *deployment) int {
	w := watchSignals()
	defer w.stop()
	for i, app := range d.flyApps {
		if err := checkUnchanged(ctx, d); err != nil {
			code := fail(s.env, err)
			s.reportLeft(d, i)
			return code
		}
		switch err := s.flyDeploy(ctx, d, app, w); {
		case errors.Is(err, errStopped) && i == 0:
			fmt.Fprintln(s.env.Stderr, msgNotDeployed)
			return 1
		case errors.Is(err, errStopped):
			s.reportLeft(d, i)
			return 1
		case err != nil:
			code := s.flyExitCode(err)
			s.reportLeft(d, i+1)
			return code
		}
		s.reportDeployed(d, app)
	}
	return 0
}

// reportLeft names each app from d.flyApps[from] on, which the run stopped before, with the command that deploys it.
// With from at 0 no app was tried, and what stopped the run has said so.
func (s *session) reportLeft(d *deployment, from int) {
	if from == 0 {
		return
	}
	repo := oneLine(d.repository.Name)
	for _, app := range d.flyApps[from:] {
		name := oneLine(app.name)
		fmt.Fprintf(s.env.Stderr, "%s wasn't deployed. Deploy it with cadre deploy %s --app %s.\n", name, repo, name)
	}
}

// checkCheckout finds the checkout, and holds its origin to the repo the person named.
func checkCheckout(ctx context.Context, d *deployment) error {
	checkout, err := git.Open(ctx)
	if err != nil {
		return d.notACheckout(err)
	}
	origin, err := checkout.Repository(ctx)
	if err != nil {
		return d.notACheckout(err)
	}
	if !strings.EqualFold(origin, d.repository.GitHubFullName) {
		return d.notACheckout(git.ErrNotACheckout)
	}
	d.checkout = checkout
	return nil
}

// notACheckout is the refusal for git.ErrNotACheckout. Any other error, such as a missing git, stays as it is.
func (d *deployment) notACheckout(err error) error {
	if errors.Is(err, git.ErrNotACheckout) {
		return fmt.Errorf("This folder isn't a checkout of %s. Run cadre deploy in one.", d.repository.GitHubFullName)
	}
	return err
}

func checkBranch(ctx context.Context, d *deployment) error {
	branch, err := d.checkout.Branch(ctx)
	if err != nil {
		return err
	}
	if branch != "main" {
		return errNotOnMain
	}
	return nil
}

func checkClean(ctx context.Context, d *deployment) error {
	clean, err := d.checkout.Clean(ctx)
	if err != nil {
		return err
	}
	if !clean {
		return errNotClean
	}
	return nil
}

// checkOrigin fetches, then holds main to origin/main. A failed fetch stops here: a stale origin/main proves nothing.
func checkOrigin(ctx context.Context, d *deployment) error {
	if err := d.checkout.Fetch(ctx); err != nil {
		return fmt.Errorf("Could not fetch origin: %w", err)
	}
	head, err := d.checkout.Head(ctx)
	if err != nil {
		return err
	}
	origin, err := d.checkout.OriginMain(ctx)
	if err != nil || origin != head {
		return errMainDiffers
	}
	d.commit = head
	return nil
}

// findApps reads each target's Fly app and picks its config, each app in full before the next. A target without a Fly
// app is no better than none. With no target, the repo has none cadre tracks, as a Vercel one takes the Vercel path, so
// the refusal names both ways to connect one.
func findApps(ctx context.Context, d *deployment) error {
	name := d.repository.Name
	if len(d.targets) == 0 {
		return fmt.Errorf("%s has no app cadre can deploy. Connect one with cadre repo set %s --fly-app <app>, or --vercel-project <id> --name <app> --vercel-dir <folder>.",
			name, name)
	}
	for _, t := range d.targets {
		app := flyAppOf(t)
		if app.app == "" {
			return fmt.Errorf("%s has no Fly app. Connect it with cadre repo set %s --fly-app <app>.", name, name)
		}
		if err := d.pickConfig(ctx, &app); err != nil {
			return err
		}
		d.flyApps = append(d.flyApps, app)
	}
	return nil
}

func flyAppOf(t api.Target) flyApp {
	app := flyApp{name: appName(t), statusURL: t.StatusURL}
	app.app, _ = t.Settings["app"].(string)
	app.stored, _ = t.Settings["config"].(string)
	return app
}

// pickConfig takes the app's stored config, cleaned as git lists paths, else fly.production.toml, else fly.toml. Only a
// committed file counts, so an ignored one is passed over. The first committed one is the choice, and it must be on disk
// too: without it, fly may fall back to the app's remote config.
func (d *deployment) pickConfig(ctx context.Context, app *flyApp) error {
	candidates, missing := []string{"fly.production.toml", "fly.toml"}, "Neither fly.production.toml nor fly.toml is"
	if app.stored != "" {
		candidates, missing = []string{filepath.Clean(app.stored)}, app.stored+" isn't"
	}
	for _, file := range candidates {
		committed, err := d.committed(ctx, file)
		if err != nil {
			return err
		}
		if committed {
			return d.useConfig(app, file)
		}
	}
	return d.noConfig(*app, missing)
}

// committed reports whether file is a tracked file inside the checkout. The clean check has passed, so tracked is
// committed.
func (d *deployment) committed(ctx context.Context, file string) (bool, error) {
	if !filepath.IsLocal(file) {
		return false, nil
	}
	return d.checkout.Tracked(ctx, file)
}

// useConfig settles on a committed file, or refuses it. It never moves on to the next candidate.
func (d *deployment) useConfig(app *flyApp, file string) error {
	found, err := d.inCheckout(file)
	if err != nil {
		return err
	}
	if !found {
		return d.noConfig(*app, file+" isn't")
	}
	app.config = file
	return nil
}

// noConfig refuses the app's config with the repo set command that sets the right one. That command sets the app its
// --name gives, else the one named after its Fly app, so it passes --name when the app's name is another.
func (d *deployment) noConfig(app flyApp, missing string) error {
	name := ""
	if app.name != app.app {
		name = " --name " + app.name
	}
	return fmt.Errorf("%s in this checkout. Set the right file with cadre repo set %s --fly-app %s%s%s.",
		missing, d.repository.Name, app.app, name, app.configFlags())
}

// configFlags are what the person passes to repo set, which replaces the target, so they keep its status URL. The person
// pastes them, so a URL that can't be quoted safely is left out for them to fill in. One that can goes before
// --fly-config, as the sentence's full stop would stick to it at the end of a pasted command.
func (a flyApp) configFlags() string {
	switch {
	case a.statusURL == "":
		return " --fly-config <file>"
	case quotable(a.statusURL):
		return " --status-url '" + a.statusURL + "' --fly-config <file>"
	default:
		return " --fly-config <file> --status-url <url> (see cadre repo show)"
	}
}

// quotableChars are the characters of a URL that mean nothing inside single quotes in POSIX shells and fish. ' and \ are
// out: fish reads \' and \\ inside them as escapes.
const quotableChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~:/?#[]@!$&()*+,;=%"

// quotable reports whether url holds only quotableChars.
func quotable(url string) bool {
	return strings.Trim(url, quotableChars) == ""
}

// inCheckout reports whether file, relative to the top level, is a file on disk, following links. A folder, or a link
// that leads nowhere or through a file, is missing. A file that can't be read is an error: the person needs to know why,
// not that it's gone.
func (d *deployment) inCheckout(file string) (bool, error) {
	info, err := os.Stat(filepath.Join(d.checkout.Dir, file))
	switch {
	case err == nil:
		return !info.IsDir(), nil
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return false, nil
	default:
		return false, err
	}
}

// findFly finds flyctl, then fly, which comes second as it is also Concourse's CLI, and asks it who is signed in.
func findFly(ctx context.Context, d *deployment) error {
	fly, err := exec.LookPath("flyctl")
	if err != nil {
		fly, err = exec.LookPath("fly")
	}
	if err != nil {
		return errFlyNotFound
	}
	if err := exec.CommandContext(ctx, fly, "auth", "whoami").Run(); err != nil {
		return errFlyNotSignedIn
	}
	d.fly = fly
	return nil
}

// settleOpenItems lists what the deploy needs first, then each line on what the commit lacks, and reports whether to go
// on: yes, or the person's answer to the one question they share. With nothing to list there is nothing to settle.
func (s *session) settleOpenItems(items []api.BeforeDeployingItem, lacking []string, yes bool) bool {
	if len(items) == 0 && len(lacking) == 0 {
		return true
	}
	out := s.progress()
	if len(items) > 0 {
		fmt.Fprintln(out, "Before deploying:")
	}
	for _, item := range items {
		fmt.Fprintf(out, "  #%d %s\n", item.Number, oneLine(item.Text))
	}
	for _, line := range lacking {
		fmt.Fprintln(out, line)
	}
	if yes {
		return true
	}
	fmt.Fprint(out, "Deploy anyway? [y/N] ")
	return s.answeredYes()
}

// oneLine replaces every rune that doesn't print with a space. Item text comes from pull request descriptions, so it is
// untrusted: an escape, a newline or a bidi override in it could forge the question.
func oneLine(text string) string {
	return strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return ' '
		}
		return r
	}, text)
}

// answeredYes reads one line. Only y and yes count, and never a line that stdin ended before finishing: with nothing
// to read, as under Claude Code, the answer is no. Nobody pressed return then, so it ends the question's line itself.
func (s *session) answeredYes() bool {
	line, err := readLine(s.env.stdin())
	if err != nil {
		fmt.Fprintln(s.progress())
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// readLine reads up to the newline one byte at a time, as a buffered read would take what follows, which is fly's. A
// line that r ends before finishing is an error.
func readLine(r io.Reader) (string, error) {
	var line []byte
	var next [1]byte
	for {
		if _, err := io.ReadFull(r, next[:]); err != nil {
			return "", err
		}
		if next[0] == '\n' {
			return string(line), nil
		}
		line = append(line, next[0])
	}
}

// checkUnchanged repeats the local checks right before fly. The question can wait for minutes, and fly ships the
// working tree, so a commit, a switch or an edit since the checks must not go out under d.commit. It makes no network
// call.
func checkUnchanged(ctx context.Context, d *deployment) error {
	if err := checkBranch(ctx, d); err != nil {
		return err
	}
	if err := checkClean(ctx, d); err != nil {
		return err
	}
	head, err := d.checkout.Head(ctx)
	if err != nil {
		return err
	}
	if head != d.commit {
		return errMainDiffers
	}
	return nil
}

// flyDeploy runs fly for app at the checkout's top level, with the commit as the image label, and waits for it. It runs
// under w, so a SIGTERM reaches it, and a signal that came first keeps it from starting, as errStopped.
func (s *session) flyDeploy(ctx context.Context, d *deployment, app flyApp, w *signalWatch) error {
	cmd := exec.CommandContext(ctx, d.fly, "deploy", "--app", app.app, "--config", app.config, "--image-label", d.commit)
	cmd.Dir = d.checkout.Dir
	cmd.Stdin = s.env.stdin()
	cmd.Stdout = s.progress()
	cmd.Stderr = s.env.Stderr
	return w.runOne(cmd)
}

// forwardSignals passes each SIGTERM on sigs to forward, as fly or vercel wouldn't otherwise receive one, until done is
// closed. SIGINT never goes through: the terminal sends it to the whole group, and a second one would make fly abort.
func forwardSignals(sigs <-chan os.Signal, done <-chan struct{}, forward func(os.Signal)) {
	for {
		select {
		case sig := <-sigs:
			if sig == syscall.SIGTERM {
				forward(sig)
			}
		case <-done:
			return
		}
	}
}

// flyExitCode is the code fly ended with, where a signal, which leaves none, reads as 1.
func (s *session) flyExitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return max(exit.ExitCode(), 1)
	}
	fmt.Fprintf(s.env.Stderr, "Could not run fly: %v\n", err)
	return 1
}

// reportDeployed names the Fly app that fly deployed, and with --json prints its object on a line of its own.
func (s *session) reportDeployed(d *deployment, app flyApp) {
	fmt.Fprintf(s.progress(), "Deployed %s to %s. Cadre shows it on its next check.\n", d.commit[:7], app.app)
	if !s.asJSON {
		return
	}
	_ = json.NewEncoder(s.env.Stdout).Encode(struct {
		Repo   string `json:"repo"`
		App    string `json:"app"`
		Config string `json:"config"`
		Commit string `json:"commit"`
	}{d.repository.Name, app.app, app.config, d.commit})
}
