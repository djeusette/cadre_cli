package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/djeusette/cadre_cli/internal/api"
)

// The Vercel refusals that need nothing from the repo. Their text is printed as it is.
var (
	errVercelNotFound    = errors.New("vercel isn't installed. See https://vercel.com/docs/cli.")
	errVercelNotSignedIn = errors.New("vercel isn't signed in. Run vercel login.")
)

// fullSHA is a whole commit name in hex, SHA-1 or SHA-256. Only such a name reaches git as a commit to compare: any
// other text could read as a ref or an option.
var fullSHA = regexp.MustCompile(`\A(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})\z`)

// vercelApp is one of the repo's Vercel targets, as cadre stores it.
type vercelApp struct {
	name, project, dir, team string
	live                     string // the commit of its last deploy, or empty when there is none to compare
}

// vercelDeployment is what the Vercel checks learn, in order, and all that the builds need. Its deployment holds the
// repo, the checkout and the commit: HEAD, which origin has.
type vercelDeployment struct {
	deployment
	apps   []vercelApp // in name order
	vercel string      // the path to vercel
}

// vercelChecks run in this order. The first to return an error ends the deploy, with that error as the refusal.
var vercelChecks = []func(context.Context, *vercelDeployment) error{
	func(ctx context.Context, v *vercelDeployment) error { return checkCheckout(ctx, &v.deployment) },
	func(ctx context.Context, v *vercelDeployment) error { return checkClean(ctx, &v.deployment) },
	checkPushed,
	findVercelApps,
	findVercel,
}

// vercelDeploy builds every Vercel app of the repo, then promotes them all, once every check has passed and the one
// question is settled. Anything else ends here.
func vercelDeploy(ctx context.Context, s *session, repo api.Repository, yes bool) int {
	v := &vercelDeployment{deployment: deployment{repository: repo}}
	for _, check := range vercelChecks {
		if err := check(ctx, v); err != nil {
			return fail(s.env, err)
		}
	}

	next, err := s.client.NextDeploy(ctx, repo.Name)
	if err != nil {
		return s.repoFailure(err, repo.Name, "")
	}
	lacking, err := v.lacking(ctx)
	if err != nil {
		return fail(s.env, err)
	}
	if !s.settleOpenItems(next.BeforeDeploying, lacking, yes) {
		fmt.Fprintln(s.env.Stderr, msgNotDeployed)
		return 1
	}
	if err := v.recheck(ctx); err != nil {
		return fail(s.env, err)
	}
	return s.buildThenPromote(ctx, v)
}

// checkPushed fetches every branch of origin, then holds HEAD to a commit origin has, from any branch, since cadre
// reads a deploy's commit from GitHub. A failed fetch stops here: a stale origin proves nothing.
func checkPushed(ctx context.Context, v *vercelDeployment) error {
	if err := v.checkout.FetchAll(ctx); err != nil {
		return fmt.Errorf("Could not fetch origin: %w", err)
	}
	head, err := v.checkout.Head(ctx)
	if err != nil {
		return err
	}
	pushed, err := v.checkout.OnOrigin(ctx, head)
	if err != nil {
		return err
	}
	if !pushed {
		return fmt.Errorf("%s isn't on origin. Push it, then deploy again.", head[:7])
	}
	v.commit = head
	return nil
}

// findVercelApps reads the repo's Vercel apps in name order, and checks each in full before the next.
func findVercelApps(_ context.Context, v *vercelDeployment) error {
	for _, t := range v.repository.Targets {
		if t.Tracked && t.Platform == "vercel" {
			v.apps = append(v.apps, vercelAppOf(t))
		}
	}
	slices.SortFunc(v.apps, func(a, b vercelApp) int { return strings.Compare(a.name, b.name) })
	for _, app := range v.apps {
		if err := v.checkApp(app); err != nil {
			return err
		}
	}
	return nil
}

func vercelAppOf(t api.Target) vercelApp {
	app := vercelApp{name: t.Name}
	app.project, _ = t.Settings["project"].(string)
	app.dir, _ = t.Settings["dir"].(string)
	if t.Account != nil {
		app.team = *t.Account
	}
	if t.LastDeploy != nil && t.LastDeploy.Commit != nil {
		app.live = *t.LastDeploy.Commit
	}
	return app
}

// checkApp refuses an app with no team to build in, a folder that isn't in the checkout, or a folder with no
// .vercelignore: vercel uploads the folder it runs in and doesn't read .gitignore, so that file alone keeps credentials
// and local files off Vercel. The no-team refusal ends with its command, so it pastes without a full stop.
func (v *vercelDeployment) checkApp(app vercelApp) error {
	name, dir := oneLine(app.name), oneLine(app.dir)
	if app.team == "" {
		return fmt.Errorf("%s has no Vercel team in cadre. Connect it again with cadre repo set %s --vercel-project %s --name %s --vercel-dir %s",
			name, v.repository.Name, oneLine(app.project), name, dir)
	}
	folder, err := v.isFolder(app.dir)
	if err != nil {
		return err
	}
	if !folder {
		return fmt.Errorf("%s isn't in this checkout.", dir)
	}
	ignoreFile, err := v.inCheckout(filepath.Join(app.dir, ".vercelignore"))
	if err != nil {
		return err
	}
	if !ignoreFile {
		return fmt.Errorf("%s has no .vercelignore. Add one that lets through only what %s's build reads.", folderName(app.dir), name)
	}
	return nil
}

// folderName is how a refusal names an app's folder: the repo's root, or the folder itself.
func folderName(dir string) string {
	if filepath.Clean(dir) == "." {
		return "The repo's root"
	}
	return oneLine(dir)
}

// isFolder reports whether dir, relative to the top level and never above it, is a folder on disk, following links.
// As with inCheckout, a folder that can't be read is an error, not a missing one.
func (d *deployment) isFolder(dir string) (bool, error) {
	if !filepath.IsLocal(dir) {
		return false, nil
	}
	info, err := os.Stat(filepath.Join(d.checkout.Dir, dir))
	switch {
	case err == nil:
		return info.IsDir(), nil
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return false, nil
	default:
		return false, err
	}
}

// findVercel finds vercel and asks it who is signed in.
func findVercel(ctx context.Context, v *vercelDeployment) error {
	vercel, err := exec.LookPath("vercel")
	if err != nil {
		return errVercelNotFound
	}
	if err := exec.CommandContext(ctx, vercel, "whoami").Run(); err != nil {
		return errVercelNotSignedIn
	}
	v.vercel = vercel
	return nil
}

// lacking is a line for each app, in name order, whose last deploy's commit the commit doesn't contain: deploying would
// take that out. An app whose last deploy has no commit has nothing to compare. A commit that isn't a full SHA, or that
// the checkout doesn't know, counts as missing.
func (v *vercelDeployment) lacking(ctx context.Context) ([]string, error) {
	var lines []string
	for _, app := range v.apps {
		if app.live == "" {
			continue
		}
		contained := false
		if fullSHA.MatchString(app.live) {
			var err error
			if contained, err = v.checkout.Contains(ctx, v.commit, app.live); err != nil {
				return nil, err
			}
		}
		if !contained {
			lines = append(lines, fmt.Sprintf("This commit lacks what %s runs now (%s).", oneLine(app.name), shortCommit(app.live)))
		}
	}
	return lines, nil
}

// shortCommit is a commit's first seven characters, on one line.
func shortCommit(commit string) string {
	runes := []rune(oneLine(commit))
	return string(runes[:min(len(runes), 7)])
}

// recheck repeats the local checks right before the builds. The question can wait for minutes, and vercel uploads the
// working tree, so a commit, a switch or an edit since the checks must not go out as v.commit. It makes no network call.
func (v *vercelDeployment) recheck(ctx context.Context) error {
	if err := checkClean(ctx, &v.deployment); err != nil {
		return err
	}
	head, err := v.checkout.Head(ctx)
	if err != nil {
		return err
	}
	if head != v.commit {
		return fmt.Errorf("HEAD is no longer at %s. Deploy again.", v.commit[:7])
	}
	return nil
}
