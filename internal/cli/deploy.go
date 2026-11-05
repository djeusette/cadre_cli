package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

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
	fly        string // the path to fly or flyctl
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

// pickConfig takes the target's config, else fly.production.toml if the checkout has one, else fly.toml. A file that
// can't be read stops the deploy, because fly.toml may be the wrong config for it.
func pickConfig(_ context.Context, d *deployment) error {
	if config, _ := d.settings["config"].(string); config != "" {
		d.config = config
		return nil
	}
	_, err := os.Stat(filepath.Join(d.checkout.Dir, "fly.production.toml"))
	switch {
	case err == nil:
		d.config = "fly.production.toml"
	case errors.Is(err, fs.ErrNotExist):
		d.config = "fly.toml"
	default:
		return err
	}
	return nil
}

// findFly finds fly, or flyctl as some installs name it, and asks it who is signed in.
func findFly(ctx context.Context, d *deployment) error {
	fly, err := exec.LookPath("fly")
	if err != nil {
		fly, err = exec.LookPath("flyctl")
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
		fmt.Fprintf(out, "  #%d %s\n", item.Number, item.Text)
	}
	if yes {
		return true
	}
	fmt.Fprint(out, "Deploy anyway? [y/N] ")
	return s.answeredYes()
}

// answeredYes reads one line. Only y and yes count, and never a line that stdin ended before finishing: with no
// terminal, as under Claude Code, the answer is no.
func (s *session) answeredYes() bool {
	line, err := bufio.NewReader(s.env.stdin()).ReadString('\n')
	if err != nil {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// flyDeploy runs fly at the checkout's top level, with the commit as the image label.
func (s *session) flyDeploy(ctx context.Context, d *deployment) int {
	cmd := exec.CommandContext(ctx, d.fly, "deploy", "--app", d.app, "--config", d.config, "--image-label", d.commit)
	cmd.Dir = d.checkout.Dir
	cmd.Stdin = s.env.stdin()
	cmd.Stdout = s.progress()
	cmd.Stderr = s.env.Stderr
	if err := cmd.Run(); err != nil {
		return s.flyExitCode(err)
	}
	s.reportDeployed(d)
	return 0
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
