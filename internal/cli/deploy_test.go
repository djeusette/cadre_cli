package cli_test

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/djeusette/cadre_cli/internal/cli"
	"github.com/djeusette/cadre_cli/internal/config"
)

// What each refusal prints on stderr.
const (
	refusedCheckout = "This folder isn't a checkout of colette-club/colette-api. Run cadre deploy in one.\n"
	refusedBranch   = "Not on main. Run git switch main.\n"
	refusedClean    = "Uncommitted changes or untracked files. Commit or stash them first.\n"
	refusedMain     = "main differs from origin/main. Run git pull, or push your commits, then deploy again.\n"
	refusedApp      = "colette-api has no Fly app. Connect it with cadre repo set colette-api --fly-app <app>.\n"
	refusedFly      = "fly isn't installed. See https://fly.io/docs/flyctl/install/.\n"
	refusedSignIn   = "fly isn't signed in. Run fly auth login.\n"
	refusedAnswer   = "Not deployed.\n"
)

// refusedConfig is the refusal for a config file the checkout doesn't have.
func refusedConfig(file string) string {
	return file + " isn't in this checkout. Set the right file with cadre repo set colette-api --fly-app colette-api-prod --fly-config <file>.\n"
}

const (
	flyTarget    = `{"platform":"fly","settings":{"app":"colette-api-prod"},"tracked":true}`
	storedTarget = `{"platform":"fly","settings":{"app":"colette-api-prod","config":"fly.custom.toml"},"tracked":true}`
	renderTarget = `{"platform":"render","settings":{},"tracked":false}`
	openItems    = `[
		{"number":418,"url":"https://github.com/colette-club/colette-api/pull/418","text":"Set SENDGRID_KEY on colette-api"},
		{"number":399,"url":"https://github.com/colette-club/colette-api/pull/399","text":"Run the backfill"}
	]`
	openItemsText  = "Before deploying:\n  #418 Set SENDGRID_KEY on colette-api\n  #399 Run the backfill\n"
	deployQuestion = "Deploy anyway? [y/N] "
)

// fakeFly stands in for fly. Only `auth whoami` answers without logging, so any other call shows in $FAKE_FLY_LOG, with
// the line it read from stdin.
const fakeFly = `#!/bin/sh
if [ "$*" = "auth whoami" ]; then
  echo "ada@colette.club"
  exit "$FAKE_FLY_AUTH"
fi
read -r line
echo "$PWD" > "$FAKE_FLY_LOG"
echo "$*" >> "$FAKE_FLY_LOG"
echo "$line" >> "$FAKE_FLY_LOG"
echo "fly output"
echo "fly warning" >&2
exit "$FAKE_FLY_EXIT"
`

// deployRig is a clean clone of colette-club/colette-api to deploy from, a Cadre server that answers for it, and a
// fake fly. PATH holds only the fake and a link to git, so no test can reach the real fly.
type deployRig struct {
	t              *testing.T
	h              *harness
	srv            *httptest.Server
	root           string
	origin         string
	clone          string
	flyDir, gitDir string
	realGit        string
	log            string

	// What the server answers.
	fullName string
	targets  string
	items    string
	repoGone bool
	next     func(http.ResponseWriter) // answers next-deploy instead of the items, when set
}

func newDeployRig(t *testing.T) *deployRig {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	r := &deployRig{
		t:        t,
		root:     root,
		origin:   filepath.Join(root, "colette-club", "colette-api.git"),
		clone:    filepath.Join(root, "colette-api"),
		flyDir:   filepath.Join(root, "fly-bin"),
		gitDir:   filepath.Join(root, "git-bin"),
		realGit:  realGit,
		log:      filepath.Join(root, "fly.log"),
		fullName: "colette-club/colette-api",
		targets:  flyTarget,
		items:    "[]",
	}
	r.isolate()
	r.makeClone()
	r.startServer()
	r.h = newHarness(t, r.srv, &config.Config{APIKey: "cadre_abc"})
	r.h.deps.Stdin = strings.NewReader("")
	t.Chdir(r.clone)
	return r
}

// isolate gives the test its own git identity and a PATH without the real fly.
func (r *deployRig) isolate() {
	r.t.Helper()
	for _, dir := range []string{r.flyDir, r.gitDir, r.origin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			r.t.Fatal(err)
		}
	}
	if err := os.Symlink(r.realGit, filepath.Join(r.gitDir, "git")); err != nil {
		r.t.Fatal(err)
	}
	r.installFly("fly", fakeFly)
	for key, value := range map[string]string{
		"PATH":                r.flyDir + string(os.PathListSeparator) + r.gitDir,
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_SYSTEM":   os.DevNull,
		"GIT_AUTHOR_NAME":     "t",
		"GIT_AUTHOR_EMAIL":    "t@example.com",
		"GIT_COMMITTER_NAME":  "t",
		"GIT_COMMITTER_EMAIL": "t@example.com",
		"FAKE_FLY_LOG":        r.log,
		"FAKE_FLY_AUTH":       "0",
		"FAKE_FLY_EXIT":       "0",
	} {
		r.t.Setenv(key, value)
	}
}

func (r *deployRig) installFly(name, script string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.flyDir, name), []byte(script), 0o755); err != nil {
		r.t.Fatal(err)
	}
}

func (r *deployRig) makeClone() {
	r.t.Helper()
	r.git(r.origin, "init", "--bare", "-b", "main")
	r.git(r.root, "clone", r.origin, r.clone)
	r.git(r.clone, "checkout", "-b", "main")
	r.commit(r.clone, "README.md")
	r.pushed("fly.toml")
}

func (r *deployRig) startServer() {
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == "/api/repositories/colette-api" && r.repoGone:
			writeJSON(w, 404, `{"error":"repository_not_found"}`)
		case req.URL.Path == "/api/repositories/colette-api":
			writeJSON(w, 200, `{"name":"colette-api","github_full_name":"`+r.fullName+`","targets":[`+r.targets+`]}`)
		case req.URL.Path == "/api/repositories/colette-api/next-deploy" && r.next != nil:
			r.next(w)
		case req.URL.Path == "/api/repositories/colette-api/next-deploy":
			writeJSON(w, 200, `{"repo":"colette-api","before_deploying":`+r.items+`}`)
		default:
			r.t.Errorf("unexpected path %s", req.URL.Path)
		}
	}))
	r.t.Cleanup(r.srv.Close)
}

func (r *deployRig) git(dir string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *deployRig) write(dir, name, content string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *deployRig) commit(dir, name string) {
	r.t.Helper()
	r.write(dir, name, name)
	r.git(dir, "add", name)
	r.git(dir, "commit", "-m", name)
}

// pushed commits name in the clone and pushes it, so the clone stays clean and level with origin.
func (r *deployRig) pushed(name string) {
	r.t.Helper()
	r.commit(r.clone, name)
	r.git(r.clone, "push", "origin", "main")
}

func (r *deployRig) head() string {
	r.t.Helper()
	return r.git(r.clone, "rev-parse", "HEAD")
}

func (r *deployRig) deploy(flags ...string) int {
	r.t.Helper()
	return cli.Run(append([]string{"deploy", "colette-api"}, flags...), r.h.deps)
}

// flyRan is where fly deploy ran, its arguments and the line it read from stdin, and fails the test when it didn't run.
func (r *deployRig) flyRan() (dir, args, stdin string) {
	r.t.Helper()
	raw, err := os.ReadFile(r.log)
	if err != nil {
		r.t.Fatalf("fly deploy never ran: %v", err)
	}
	lines := strings.SplitN(string(raw), "\n", 3)
	return lines[0], lines[1], strings.TrimSpace(lines[2])
}

func (r *deployRig) assertNoDeploy() {
	r.t.Helper()
	raw, err := os.ReadFile(r.log)
	if !errors.Is(err, fs.ErrNotExist) {
		r.t.Errorf("fly deploy ran: %q (%v)", raw, err)
	}
}

// assertRefused checks that the run printed only want, on stderr, and deployed nothing.
func (r *deployRig) assertRefused(code int, want string) {
	r.t.Helper()
	if code != 1 {
		r.t.Errorf("code = %d, want 1", code)
	}
	if r.h.errb.String() != want {
		r.t.Errorf("stderr = %q, want %q", r.h.errb, want)
	}
	if r.h.out.Len() != 0 {
		r.t.Errorf("stdout = %q", r.h.out)
	}
	r.assertNoDeploy()
}

// gitFailing is a PATH whose git fails on one call and hands every other to the real git.
func (r *deployRig) gitFailing(call string) string {
	r.t.Helper()
	wrapper := filepath.Join(r.root, "git-wrapper")
	if err := os.Mkdir(wrapper, 0o755); err != nil {
		r.t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$*\" = '%s' ]; then\n  echo 'git broke' >&2\n  exit 1\nfi\nexec '%s' \"$@\"\n", call, r.realGit)
	if err := os.WriteFile(filepath.Join(wrapper, "git"), []byte(script), 0o755); err != nil {
		r.t.Fatal(err)
	}
	return wrapper + string(os.PathListSeparator) + r.flyDir
}

func deployedLine(sha string) string {
	return "Deployed " + sha[:7] + " to colette-api-prod. Cadre shows it on its next check.\n"
}

// Each fault breaks one check of a clean rig.

func (r *deployRig) anotherRepo() {
	r.git(r.clone, "remote", "set-url", "origin", filepath.Join(r.root, "colette-club", "colette-web.git"))
}
func (r *deployRig) outsideGit()    { r.t.Chdir(r.t.TempDir()) }
func (r *deployRig) noOrigin()      { r.git(r.clone, "remote", "remove", "origin") }
func (r *deployRig) anotherBranch() { r.git(r.clone, "switch", "-c", "feature") }
func (r *deployRig) detachedHead()  { r.git(r.clone, "switch", "--detach") }
func (r *deployRig) stagedChange() {
	r.write(r.clone, "staged.txt", "staged")
	r.git(r.clone, "add", "staged.txt")
}
func (r *deployRig) unstagedChange() { r.write(r.clone, "README.md", "changed") }
func (r *deployRig) untrackedFile()  { r.write(r.clone, "untracked.txt", "untracked") }
func (r *deployRig) localCommit()    { r.commit(r.clone, "local.txt") }
func (r *deployRig) commitOnOrigin() {
	second := filepath.Join(r.root, "second")
	r.git(r.root, "clone", r.origin, second)
	r.commit(second, "theirs.txt")
	r.git(second, "push", "origin", "main")
}
func (r *deployRig) originGone() {
	if err := os.RemoveAll(r.origin); err != nil {
		r.t.Fatal(err)
	}
}
func (r *deployRig) onlyRender() { r.targets = renderTarget }
func (r *deployRig) noTargets()  { r.targets = "" }
func (r *deployRig) noAppSet()   { r.targets = `{"platform":"fly","settings":{},"tracked":true}` }
func (r *deployRig) noConfigFile() {
	r.git(r.clone, "rm", "-q", "fly.toml")
	r.git(r.clone, "commit", "-m", "no config")
	r.git(r.clone, "push", "origin", "main")
}
func (r *deployRig) storedConfigMissing() { r.targets = storedTarget }
func (r *deployRig) flyMissing()          { r.t.Setenv("PATH", r.gitDir) }
func (r *deployRig) flySignedOut()        { r.t.Setenv("FAKE_FLY_AUTH", "1") }
func (r *deployRig) cadreFails()          { r.next = func(w http.ResponseWriter) { writeJSON(w, 500, `{}`) } }
func (r *deployRig) repoUnknown()         { r.repoGone = true }

func TestDeployRunsFlyWithTheCommitAsTheLabel(t *testing.T) {
	r := newDeployRig(t)
	sha := r.head()

	code := r.deploy()

	if code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	if _, args, _ := r.flyRan(); args != "deploy --app colette-api-prod --config fly.toml --image-label "+sha {
		t.Errorf("fly ran with %q", args)
	}
	if want := "fly output\n" + deployedLine(sha); r.h.out.String() != want {
		t.Errorf("stdout = %q, want %q", r.h.out, want)
	}
	if r.h.errb.String() != "fly warning\n" {
		t.Errorf("stderr = %q, want only fly's", r.h.errb)
	}
}

// fly can read what it asks for, and what it says on stderr reaches the person.
func TestDeployGivesFlyItsStdinAndStderr(t *testing.T) {
	r := newDeployRig(t)
	r.h.deps.Stdin = strings.NewReader("a line for fly\n")

	if code := r.deploy(); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	if _, _, stdin := r.flyRan(); stdin != "a line for fly" {
		t.Errorf("fly read %q from stdin", stdin)
	}
	if !strings.Contains(r.h.errb.String(), "fly warning") {
		t.Errorf("stderr = %q, want fly's", r.h.errb)
	}
}

func TestDeployPicksTheConfig(t *testing.T) {
	for _, tc := range []struct {
		name, target string
		committed    []string
		want         string
	}{
		{"a stored config wins over a committed file", storedTarget, []string{"fly.custom.toml", "fly.production.toml"}, "fly.custom.toml"},
		{"fly.production.toml wins over fly.toml", flyTarget, []string{"fly.production.toml"}, "fly.production.toml"},
		{"fly.toml alone", flyTarget, nil, "fly.toml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDeployRig(t)
			r.targets = tc.target
			for _, name := range tc.committed {
				r.pushed(name)
			}

			if code := r.deploy(); code != 0 {
				t.Fatalf("code = %d, stderr %q", code, r.h.errb)
			}
			if _, args, _ := r.flyRan(); !strings.Contains(args, " --config "+tc.want+" ") {
				t.Errorf("fly ran with %q, want --config %s", args, tc.want)
			}
		})
	}
}

// A file that can't be read isn't a file that isn't there: guessing fly.toml could deploy the wrong config.
func TestDeployWhenTheConfigCantBeRead(t *testing.T) {
	r := newDeployRig(t)
	if err := os.Symlink("fly.production.toml", filepath.Join(r.clone, "fly.production.toml")); err != nil {
		t.Fatal(err)
	}
	r.git(r.clone, "add", "fly.production.toml")
	r.git(r.clone, "commit", "-m", "a link to itself")
	r.git(r.clone, "push", "origin", "main")

	code := r.deploy()

	if code != 1 || !strings.Contains(r.h.errb.String(), "fly.production.toml") {
		t.Errorf("code = %d, stderr %q", code, r.h.errb)
	}
	r.assertNoDeploy()
}

func TestDeployRunsAtTheTopLevel(t *testing.T) {
	r := newDeployRig(t)
	r.pushed("fly.production.toml")
	sub := filepath.Join(r.clone, "lib", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	if code := r.deploy(); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	dir, args, _ := r.flyRan()
	if dir != r.clone {
		t.Errorf("fly ran in %q, want %q", dir, r.clone)
	}
	if !strings.Contains(args, " --config fly.production.toml ") {
		t.Errorf("fly ran with %q", args)
	}
}

func TestDeployExitsWithFlysStatus(t *testing.T) {
	r := newDeployRig(t)
	t.Setenv("FAKE_FLY_EXIT", "3")

	code := r.deploy()

	if code != 3 {
		t.Errorf("code = %d, want 3", code)
	}
	r.flyRan()
	if r.h.out.String() != "fly output\n" {
		t.Errorf("stdout = %q, want only fly's output", r.h.out)
	}
}

func TestDeployWhenFlyIsKilled(t *testing.T) {
	r := newDeployRig(t)
	r.installFly("fly", "#!/bin/sh\n[ \"$1\" = auth ] && exit 0\nkill -KILL $$\n")

	code := r.deploy()

	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if strings.Contains(r.h.out.String(), "Deployed") {
		t.Errorf("stdout = %q", r.h.out)
	}
}

func TestDeployWhenFlyCantStart(t *testing.T) {
	r := newDeployRig(t)
	r.installFly("fly", "#!/bin/sh\n/bin/rm \"$0\"\n") // gone by the time cadre runs fly deploy

	code := r.deploy()

	if code != 1 || !strings.HasPrefix(r.h.errb.String(), "Could not run fly: ") {
		t.Errorf("code = %d, stderr %q", code, r.h.errb)
	}
	if r.h.out.Len() != 0 {
		t.Errorf("stdout = %q", r.h.out)
	}
}

func TestDeployFallsBackToFlyctl(t *testing.T) {
	r := newDeployRig(t)
	if err := os.Remove(filepath.Join(r.flyDir, "fly")); err != nil {
		t.Fatal(err)
	}
	r.installFly("flyctl", fakeFly)

	if code := r.deploy(); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	r.flyRan()
}

func TestDeployRefuses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault func(*deployRig)
		want  string
	}{
		{"a clone of another repo", (*deployRig).anotherRepo, refusedCheckout},
		{"a folder outside git", (*deployRig).outsideGit, refusedCheckout},
		{"a clone with no origin", (*deployRig).noOrigin, refusedCheckout},
		{"another branch", (*deployRig).anotherBranch, refusedBranch},
		{"a detached HEAD", (*deployRig).detachedHead, refusedBranch},
		{"a staged change", (*deployRig).stagedChange, refusedClean},
		{"an unstaged change", (*deployRig).unstagedChange, refusedClean},
		{"an untracked file", (*deployRig).untrackedFile, refusedClean},
		{"a local commit", (*deployRig).localCommit, refusedMain},
		{"a commit pushed from a second clone", (*deployRig).commitOnOrigin, refusedMain},
		{"targets with only render", (*deployRig).onlyRender, refusedApp},
		{"no targets", (*deployRig).noTargets, refusedApp},
		{"a fly target with no app", (*deployRig).noAppSet, refusedApp},
		{"a stored config the checkout doesn't have", (*deployRig).storedConfigMissing, refusedConfig("fly.custom.toml")},
		{"neither config file", (*deployRig).noConfigFile, refusedConfig("fly.toml")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDeployRig(t)
			tc.fault(r)

			r.assertRefused(r.deploy(), tc.want)
		})
	}
}

// Two faults at once: the earlier check is the one that speaks.
func TestDeployChecksInOrder(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second func(*deployRig)
		want          string
	}{
		{"the repo, then the checkout", (*deployRig).repoUnknown, (*deployRig).outsideGit, "No repo colette-api on "},
		{"the checkout, then the branch", (*deployRig).anotherRepo, (*deployRig).anotherBranch, refusedCheckout},
		{"the branch, then clean", (*deployRig).anotherBranch, (*deployRig).untrackedFile, refusedBranch},
		{"clean, then the fetch", (*deployRig).untrackedFile, (*deployRig).originGone, refusedClean},
		{"the fetch, then main", (*deployRig).originGone, (*deployRig).localCommit, "Could not fetch origin: "},
		{"main, then the Fly app", (*deployRig).localCommit, (*deployRig).onlyRender, refusedMain},
		{"the Fly app, then the config", (*deployRig).onlyRender, (*deployRig).noConfigFile, refusedApp},
		{"the config, then fly", (*deployRig).noConfigFile, (*deployRig).flyMissing, refusedConfig("fly.toml")},
		{"fly, then the open items", (*deployRig).flyMissing, (*deployRig).cadreFails, refusedFly},
		{"signed in, then the open items", (*deployRig).flySignedOut, (*deployRig).cadreFails, refusedSignIn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDeployRig(t)
			tc.first(r)
			tc.second(r)

			code := r.deploy()

			if code != 1 || !strings.HasPrefix(r.h.errb.String(), tc.want) {
				t.Errorf("code = %d, stderr %q, want %q", code, r.h.errb, tc.want)
			}
			r.assertNoDeploy()
		})
	}
}

func TestDeployMatchesTheRepoInAnyCase(t *testing.T) {
	r := newDeployRig(t)
	r.fullName = "Colette-Club/Colette-API"

	if code := r.deploy(); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	r.flyRan()
}

func TestDeployIgnoresIgnoredFiles(t *testing.T) {
	r := newDeployRig(t)
	r.write(r.clone, ".gitignore", "ignored.log\n")
	r.git(r.clone, "add", ".gitignore")
	r.git(r.clone, "commit", "-m", "ignore")
	r.git(r.clone, "push", "origin", "main")
	r.write(r.clone, "ignored.log", "ignored")

	if code := r.deploy(); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	r.flyRan()
}

func TestDeployWhenFetchFails(t *testing.T) {
	r := newDeployRig(t)
	r.originGone()

	code := r.deploy()

	if code != 1 || !strings.HasPrefix(r.h.errb.String(), "Could not fetch origin: ") {
		t.Errorf("code = %d, stderr %q", code, r.h.errb)
	}
	if strings.TrimPrefix(r.h.errb.String(), "Could not fetch origin: ") == "\n" {
		t.Errorf("stderr = %q, want git's error", r.h.errb)
	}
	r.assertNoDeploy()
}

// A git call that fails stops the deploy: it never reads as clean, or as main matching.
func TestDeployWhenGitFails(t *testing.T) {
	for _, tc := range []struct {
		failing string
		want    string
	}{
		{"branch --show-current", "git broke\n"},
		{"status --porcelain --untracked-files=normal", "git broke\n"},
		{"fetch origin", "Could not fetch origin: git broke\n"},
		{"rev-parse HEAD", "git broke\n"},
		{"rev-parse refs/remotes/origin/main", refusedMain},
	} {
		t.Run(tc.failing, func(t *testing.T) {
			r := newDeployRig(t)
			t.Setenv("PATH", r.gitFailing(tc.failing))

			r.assertRefused(r.deploy(), tc.want)
		})
	}
}

// git missing is not "this folder isn't a checkout": the error is printed as it is.
func TestDeployWithoutGit(t *testing.T) {
	r := newDeployRig(t)
	t.Setenv("PATH", r.flyDir)

	code := r.deploy()

	if code != 1 || !strings.Contains(r.h.errb.String(), "executable file not found") {
		t.Errorf("code = %d, stderr %q", code, r.h.errb)
	}
	r.assertNoDeploy()
}

func TestDeployWithoutFly(t *testing.T) {
	r := newDeployRig(t)
	r.flyMissing()

	r.assertRefused(r.deploy(), refusedFly)
}

func TestDeployWhenFlyIsNotSignedIn(t *testing.T) {
	r := newDeployRig(t)
	r.flySignedOut()

	r.assertRefused(r.deploy(), refusedSignIn)
}

func TestDeployAsksAboutOpenItems(t *testing.T) {
	for _, answer := range []string{"y\n", "Y\n", "yes\n", " Yes \n"} {
		t.Run(fmt.Sprintf("%q deploys", answer), func(t *testing.T) {
			r := newDeployRig(t)
			r.items = openItems
			r.h.deps.Stdin = strings.NewReader(answer)

			code := r.deploy()

			if code != 0 {
				t.Fatalf("code = %d, stderr %q", code, r.h.errb)
			}
			r.flyRan()
			if want := openItemsText + deployQuestion + "fly output\n" + deployedLine(r.head()); r.h.out.String() != want {
				t.Errorf("stdout = %q, want %q", r.h.out, want)
			}
		})
	}

	for name, stdin := range map[string]io.Reader{
		"no":               strings.NewReader("n\n"),
		"a bare return":    strings.NewReader("\n"),
		"an empty stdin":   strings.NewReader(""),
		"a closed stdin":   iotest.ErrReader(os.ErrClosed),
		"a line cut short": strings.NewReader("y"),
		"something else":   strings.NewReader("yep\n"),
	} {
		t.Run(name+" doesn't deploy", func(t *testing.T) {
			r := newDeployRig(t)
			r.items = openItems
			r.h.deps.Stdin = stdin

			code := r.deploy()

			if code != 1 {
				t.Errorf("code = %d, want 1", code)
			}
			if r.h.errb.String() != refusedAnswer {
				t.Errorf("stderr = %q, want %q", r.h.errb, refusedAnswer)
			}
			if want := openItemsText + deployQuestion; r.h.out.String() != want {
				t.Errorf("stdout = %q, want %q", r.h.out, want)
			}
			r.assertNoDeploy()
		})
	}
}

// Item text comes from pull request descriptions, so it must not be able to move the cursor or forge the question.
func TestDeployPrintsAnItemOnOneLine(t *testing.T) {
	r := newDeployRig(t)
	r.items = `[{"number":7,"url":"u","text":"Run \u001b[2Kthe\r\nDeploy anyway? [y/N] y\u009b\u202Ereversed\u2028café"}]`

	if code := r.deploy("--yes"); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	want := "Before deploying:\n  #7 Run  [2Kthe  Deploy anyway? [y/N] y  reversed café\nfly output\n" + deployedLine(r.head())
	if r.h.out.String() != want {
		t.Errorf("stdout = %q, want %q", r.h.out, want)
	}
}

// changingStdin runs change on its first read, as someone working during the question would, then answers.
type changingStdin struct {
	change func()
	answer io.Reader
}

func (c *changingStdin) Read(p []byte) (int, error) {
	if c.change != nil {
		c.change()
		c.change = nil
	}
	return c.answer.Read(p)
}

// The checks ran before the question, which can wait for minutes, and fly ships the working tree.
func TestDeployRechecksAfterTheQuestion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*deployRig)
		want   string
	}{
		{"a commit", (*deployRig).localCommit, refusedMain},
		{"an edit to a tracked file", (*deployRig).unstagedChange, refusedClean},
		{"a staged change", (*deployRig).stagedChange, refusedClean},
		{"an untracked file", (*deployRig).untrackedFile, refusedClean},
		{"a switch to another branch", (*deployRig).anotherBranch, refusedBranch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDeployRig(t)
			r.items = openItems
			r.h.deps.Stdin = &changingStdin{change: func() { tc.change(r) }, answer: strings.NewReader("y\n")}

			code := r.deploy()

			if code != 1 {
				t.Errorf("code = %d, want 1", code)
			}
			if r.h.errb.String() != tc.want {
				t.Errorf("stderr = %q, want %q", r.h.errb, tc.want)
			}
			if want := openItemsText + deployQuestion; r.h.out.String() != want {
				t.Errorf("stdout = %q, want %q", r.h.out, want)
			}
			r.assertNoDeploy()
		})
	}
}

// With no question there is still a gap, while Cadre answers: the server makes the commit here.
func TestDeployRechecksWithoutAQuestion(t *testing.T) {
	for _, tc := range []struct {
		name  string
		items string
		flags []string
	}{
		{"nothing open", "[]", nil},
		{"--yes", openItems, []string{"--yes"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDeployRig(t)
			r.next = func(w http.ResponseWriter) {
				r.localCommit()
				writeJSON(w, 200, `{"repo":"colette-api","before_deploying":`+tc.items+`}`)
			}

			code := r.deploy(tc.flags...)

			if code != 1 || r.h.errb.String() != refusedMain {
				t.Errorf("code = %d, stderr %q, want %q", code, r.h.errb, refusedMain)
			}
			r.assertNoDeploy()
		})
	}
}

// git breaking only once the question is answered stops the deploy like any other git failure.
func TestDeployWhenGitFailsAtTheRecheck(t *testing.T) {
	for _, failing := range []string{
		"branch --show-current",
		"status --porcelain --untracked-files=normal",
		"rev-parse HEAD",
	} {
		t.Run(failing, func(t *testing.T) {
			r := newDeployRig(t)
			r.items = openItems
			broken := r.gitFailing(failing)
			r.h.deps.Stdin = &changingStdin{change: func() { t.Setenv("PATH", broken) }, answer: strings.NewReader("y\n")}

			code := r.deploy()

			if code != 1 || r.h.errb.String() != "git broke\n" {
				t.Errorf("code = %d, stderr %q", code, r.h.errb)
			}
			r.assertNoDeploy()
		})
	}
}

func TestDeployYesGoesOnWithoutAsking(t *testing.T) {
	r := newDeployRig(t)
	r.items = openItems

	code := r.deploy("--yes")

	if code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	r.flyRan()
	if want := openItemsText + "fly output\n" + deployedLine(r.head()); r.h.out.String() != want {
		t.Errorf("stdout = %q, want %q", r.h.out, want)
	}
}

// Cadre not answering never reads as "nothing to do", even with --yes.
func TestDeployWhenCadreCantAnswer(t *testing.T) {
	for name, answer := range map[string]func(http.ResponseWriter){
		"a 500":              func(w http.ResponseWriter) { writeJSON(w, 500, `{}`) },
		"a dropped request":  func(http.ResponseWriter) { panic(http.ErrAbortHandler) },
		"an unreadable body": func(w http.ResponseWriter) { writeJSON(w, 200, `not json`) },
		"an empty object":    func(w http.ResponseWriter) { writeJSON(w, 200, `{}`) },
		"a null list":        func(w http.ResponseWriter) { writeJSON(w, 200, `{"repo":"colette-api","before_deploying":null}`) },
		"a renamed key":      func(w http.ResponseWriter) { writeJSON(w, 200, `{"repo":"colette-api","open_items":[]}`) },
	} {
		for _, flags := range [][]string{nil, {"--yes"}} {
			t.Run(fmt.Sprintf("%s %v", name, flags), func(t *testing.T) {
				r := newDeployRig(t)
				r.next = answer

				code := r.deploy(flags...)

				if code != 1 || r.h.errb.Len() == 0 {
					t.Errorf("code = %d, stderr %q", code, r.h.errb)
				}
				if r.h.out.Len() != 0 {
					t.Errorf("stdout = %q", r.h.out)
				}
				r.assertNoDeploy()
			})
		}
	}
}

func TestDeployUnknownRepo(t *testing.T) {
	r := newDeployRig(t)
	r.repoUnknown()

	r.assertRefused(r.deploy(), "No repo colette-api on "+r.srv.URL+".\n")
}

func TestDeployJSON(t *testing.T) {
	r := newDeployRig(t)
	sha := r.head()

	code := r.deploy("--json")

	if code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	want := `{"repo":"colette-api","app":"colette-api-prod","config":"fly.toml","commit":"` + sha + `"}` + "\n"
	if r.h.out.String() != want {
		t.Errorf("stdout = %q, want %q", r.h.out, want)
	}
	if want := "fly output\nfly warning\n" + deployedLine(sha); r.h.errb.String() != want {
		t.Errorf("stderr = %q, want %q", r.h.errb, want)
	}
}

func TestDeployNotSignedIn(t *testing.T) {
	r := newDeployRig(t)
	r.h = newHarness(t, r.srv, nil)

	r.assertRefused(r.deploy(), "Not signed in. Run cadre login --url <url>.\n")
}
