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
	"os/signal"
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
	checkout   git.Checkout
	commit     string         // the full commit main and origin/main agree on
	settings   map[string]any // the fly target's
	app        string
	config     string
	fly        string // the path to flyctl or fly
}

// deployChecks run in this order. The first to return an error ends the deploy, with that error as the refusal.
var deployChecks = []func(context.Context, *deployment) error{
	checkCheckout,
	checkBranch,
	checkClean,
	checkOrigin,
	findApp,
	pickConfig,
	findFly,
}

// runDeploy dispatches `deploy <repo> [--yes]`.
func runDeploy(env Env, asJSON bool, args []string) int {
	args, yes := splitBoolFlag(args, "--yes")
	return runCommand(env, asJSON, "deploy", args, 1, func(ctx context.Context, s *session, a []string) int {
		return deploy(ctx, s, a[0], yes)
	})
}

// deploy runs fly only once every check has passed and the open items are settled. Anything else ends here.
func deploy(ctx context.Context, s *session, repo string, yes bool) int {
	if !s.requireKey() {
		return 1
	}
	shown, err := s.client.ShowRepository(ctx, repo)
	if err != nil {
		return s.repoFailure(err, repo, "")
	}

	d := &deployment{repository: shown}
	for _, check := range deployChecks {
		if err := check(ctx, d); err != nil {
			return fail(s.env, err)
		}
	}

	next, err := s.client.NextDeploy(ctx, repo)
	if err != nil {
		return s.repoFailure(err, repo, "")
	}
	if !s.settleOpenItems(next.BeforeDeploying, yes) {
		fmt.Fprintln(s.env.Stderr, msgNotDeployed)
		return 1
	}
	if err := checkUnchanged(ctx, d); err != nil {
		return fail(s.env, err)
	}
	return s.flyDeploy(ctx, d)
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

// findApp reads the fly target's app. A fly target without one is no better than no fly target.
func findApp(_ context.Context, d *deployment) error {
	targets := d.repository.Targets
	if i := slices.IndexFunc(targets, func(t api.Target) bool { return t.Platform == "fly" }); i >= 0 {
		d.settings = targets[i].Settings
	}
	app, _ := d.settings["app"].(string)
	if app == "" {
		name := d.repository.Name
		return fmt.Errorf("%s has no Fly app. Connect it with cadre repo set %s --fly-app <app>.", name, name)
	}
	d.app = app
	return nil
}

// pickConfig takes the target's config, cleaned as git lists paths, else fly.production.toml, else fly.toml. Only a
// committed file counts, so an ignored one is passed over. The first committed one is the choice, and it must be on disk
// too: without it, fly may fall back to the app's remote config.
func pickConfig(ctx context.Context, d *deployment) error {
	candidates, missing := []string{"fly.production.toml", "fly.toml"}, "Neither fly.production.toml nor fly.toml is"
	if stored, _ := d.settings["config"].(string); stored != "" {
		candidates, missing = []string{filepath.Clean(stored)}, stored+" isn't"
	}
	for _, file := range candidates {
		committed, err := d.committed(ctx, file)
		if err != nil {
			return err
		}
		if committed {
			return d.useConfig(file)
		}
	}
	return d.noConfig(missing)
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
func (d *deployment) useConfig(file string) error {
	found, err := d.inCheckout(file)
	if err != nil {
		return err
	}
	if !found {
		return d.noConfig(file + " isn't")
	}
	d.config = file
	return nil
}

func (d *deployment) noConfig(missing string) error {
	return fmt.Errorf("%s in this checkout. Set the right file with cadre repo set %s --fly-app %s --fly-config <file>.",
		missing, d.repository.Name, d.app)
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

// settleOpenItems lists what the deploy needs first and reports whether to go on: yes, or the person's answer.
// With no items there is nothing to settle.
func (s *session) settleOpenItems(items []api.BeforeDeployingItem, yes bool) bool {
	if len(items) == 0 {
		return true
	}
	out := s.progress()
	fmt.Fprintln(out, "Before deploying:")
	for _, item := range items {
		fmt.Fprintf(out, "  #%d %s\n", item.Number, oneLine(item.Text))
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

// flyDeploy runs fly at the checkout's top level, with the commit as the image label.
func (s *session) flyDeploy(ctx context.Context, d *deployment) int {
	cmd := exec.CommandContext(ctx, d.fly, "deploy", "--app", d.app, "--config", d.config, "--image-label", d.commit)
	cmd.Dir = d.checkout.Dir
	cmd.Stdin = s.env.stdin()
	cmd.Stdout = s.progress()
	cmd.Stderr = s.env.Stderr
	if err := runOutlivingSignals(cmd); err != nil {
		return s.flyExitCode(err)
	}
	s.reportDeployed(d)
	return 0
}

// runOutlivingSignals runs cmd with cadre staying up through SIGINT and SIGTERM until it ends, so the status is cmd's.
// Dying first would leave a deploy running with nobody to report how it went.
func runOutlivingSignals(cmd *exec.Cmd) error {
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)

	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)
	go forwardSignals(sigs, done, func(sig os.Signal) { _ = cmd.Process.Signal(sig) })
	return cmd.Wait()
}

// forwardSignals passes each SIGTERM on sigs to forward, as fly wouldn't otherwise receive one, until done is closed.
// SIGINT never goes through: the terminal sends it to the whole group, and a second one would make fly abort.
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

func (s *session) reportDeployed(d *deployment) {
	fmt.Fprintf(s.progress(), "Deployed %s to %s. Cadre shows it on its next check.\n", d.commit[:7], d.app)
	if !s.asJSON {
		return
	}
	_ = json.NewEncoder(s.env.Stdout).Encode(struct {
		Repo   string `json:"repo"`
		App    string `json:"app"`
		Config string `json:"config"`
		Commit string `json:"commit"`
	}{d.repository.Name, d.app, d.config, d.commit})
}
