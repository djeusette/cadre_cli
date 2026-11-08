package cli_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
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
	refusedMarked   = "Files marked skip-worktree or assume-unchanged can hide changes. Clear the marks, listed by git ls-files -v.\n"
	refusedMain     = "main differs from origin/main. Run git pull, or push your commits, then deploy again.\n"
	refusedApp      = "colette-api has no Fly app. Connect it with cadre repo set colette-api --fly-app <app>.\n"
	refusedNoTarget = "colette-api has no app cadre can deploy. Connect one with cadre repo set colette-api --fly-app <app>, or --vercel-project <id> --name <app> --vercel-dir <folder>.\n"
	refusedFly      = "fly isn't installed. See https://fly.io/docs/flyctl/install/.\n"
	refusedSignIn   = "fly isn't signed in. Run fly auth login.\n"
	refusedAnswer   = "Not deployed.\n"
)

const refusedNoConfig = "Neither fly.production.toml nor fly.toml is in this checkout. Set the right file with cadre repo set colette-api --fly-app colette-api-prod --fly-config <file>.\n"

// refusedConfig is the refusal for a stored config that doesn't qualify, or a committed default that isn't on disk.
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

// fakeFly stands in for fly. Only `auth whoami` answers without logging, so any other call adds three lines to
// $FAKE_FLY_LOG: where it ran, its arguments and the line it read from stdin. When $FAKE_FLY_LEAVES is set, each call
// leaves that file behind, as an edit made while fly runs would.
const fakeFly = `#!/bin/sh
if [ "$*" = "auth whoami" ]; then
  echo "ada@colette.club"
  exit "$FAKE_FLY_AUTH"
fi
read -r line
echo "$PWD" >> "$FAKE_FLY_LOG"
echo "$*" >> "$FAKE_FLY_LOG"
echo "$line" >> "$FAKE_FLY_LOG"
echo "fly output"
echo "fly warning" >&2
[ -n "$FAKE_FLY_LEAVES" ] && echo left > "$FAKE_FLY_LEAVES"
exit "$FAKE_FLY_EXIT"
`

// signalledFly sends $FAKE_FLY_SIGNAL to cadre, its parent, then gives cadre time to pass it on. A signal it gets shows
// in $FAKE_FLY_LOG, and each way of ending has its own status.
const signalledFly = `#!/bin/sh
[ "$1" = auth ] && exit 0
trap 'echo TERM > "$FAKE_FLY_LOG"; exit 7' TERM
trap 'echo INT > "$FAKE_FLY_LOG"; exit 8' INT
kill -"$FAKE_FLY_SIGNAL" "$PPID"
for i in 1 2 3 4 5 6 7 8 9 10; do /bin/sleep 0.05; done
exit 9
`

// signallingFly logs each deploy as fakeFly does. On its first, it sends $FAKE_FLY_SIGNAL to cadre, its parent, and
// gives cadre time to pass it on. Every deploy ends well, a SIGTERM's included.
const signallingFly = `#!/bin/sh
[ "$1" = auth ] && exit 0
read -r line
echo "$PWD" >> "$FAKE_FLY_LOG"
echo "$*" >> "$FAKE_FLY_LOG"
echo "$line" >> "$FAKE_FLY_LOG"
trap 'exit 0' TERM
if [ ! -f "$FAKE_FLY_LOG.signalled" ]; then
  : > "$FAKE_FLY_LOG.signalled"
  kill -"$FAKE_FLY_SIGNAL" "$PPID"
  for i in 1 2 3 4 5 6 7 8 9 10; do /bin/sleep 0.05; done
fi
exit 0
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
	name     string // the repo's name in its answer, which next-deploy goes by
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
		name:     "colette-api",
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
		"FAKE_FLY_LEAVES":     "",
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
			name, _ := json.Marshal(r.name)
			writeJSON(w, 200, `{"name":`+string(name)+`,"github_full_name":"`+r.fullName+`","targets":[`+r.targets+`]}`)
		case req.URL.Path == "/api/repositories/"+r.name+"/next-deploy" && r.next != nil:
			r.next(w)
		case req.URL.Path == "/api/repositories/"+r.name+"/next-deploy":
			writeJSON(w, 200, `{"repo":"colette-api","before_deploying":`+r.items+`}`)
		default:
			r.t.Errorf("unexpected path %s", req.URL.Path)
		}
	}))
	r.t.Cleanup(r.srv.Close)
}

func (r *deployRig) git(dir string, args ...string) string {
	r.t.Helper()
	out, err := tryGit(dir, args...)
	if err != nil {
		r.t.Fatal(err)
	}
	return out
}

// tryGit returns git's failure instead of failing the test, for a server handler: Fatal only works on the test's own
// goroutine.
func tryGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
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

// ignored commits a .gitignore for name and leaves name on disk, so the clone stays clean and level with origin.
func (r *deployRig) ignored(name string) {
	r.t.Helper()
	r.write(r.clone, ".gitignore", name+"\n")
	r.git(r.clone, "add", ".gitignore")
	r.git(r.clone, "commit", "-m", "ignore "+name)
	r.git(r.clone, "push", "origin", "main")
	r.write(r.clone, name, name)
}

// danglingLink commits name as a link that leads nowhere.
func (r *deployRig) danglingLink(name string) {
	r.t.Helper()
	r.link(name, "nowhere.toml")
}

// linkThroughAFile commits name as a link whose path goes through a file, which stat reads as ENOTDIR.
func (r *deployRig) linkThroughAFile(name string) {
	r.t.Helper()
	r.link(name, "README.md/fly.toml")
}

func (r *deployRig) link(name, target string) {
	r.t.Helper()
	if err := os.Symlink(target, filepath.Join(r.clone, name)); err != nil {
		r.t.Fatal(err)
	}
	r.git(r.clone, "add", name)
	r.git(r.clone, "commit", "-m", name)
	r.git(r.clone, "push", "origin", "main")
}

// pushedInFolder commits and pushes file in folder, and returns its path from the top level.
func (r *deployRig) pushedInFolder(folder, file string) string {
	r.t.Helper()
	if err := os.Mkdir(filepath.Join(r.clone, folder), 0o755); err != nil {
		r.t.Fatal(err)
	}
	name := folder + "/" + file
	r.pushed(name)
	return name
}

func (r *deployRig) storedConfig(file string) {
	r.targets = `{"platform":"fly","settings":{"app":"colette-api-prod","config":"` + file + `"},"tracked":true}`
}

func (r *deployRig) head() string {
	r.t.Helper()
	return r.git(r.clone, "rev-parse", "HEAD")
}

func (r *deployRig) deploy(flags ...string) int {
	r.t.Helper()
	return cli.Run(append([]string{"deploy", "colette-api"}, flags...), r.h.deps)
}

// flyRan is where the first fly deploy ran, its arguments and the line it read from stdin, and fails the test when it
// didn't run.
func (r *deployRig) flyRan() (dir, args, stdin string) {
	r.t.Helper()
	raw, err := os.ReadFile(r.log)
	if err != nil {
		r.t.Fatalf("fly deploy never ran: %v", err)
	}
	lines := strings.SplitN(string(raw), "\n", 3)
	return lines[0], lines[1], strings.TrimSpace(lines[2])
}

// flyDeploys is the arguments of each fly deploy, in the order they ran, or none when fly never deployed.
func (r *deployRig) flyDeploys() []string {
	r.t.Helper()
	raw, err := os.ReadFile(r.log)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		r.t.Fatal(err)
	}
	var deploys []string
	for i, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		if i%3 == 1 {
			deploys = append(deploys, line)
		}
	}
	return deploys
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

// gitSignalling is a PATH whose git sends SIGINT to cadre, its parent, on one call, gives cadre time to catch it, then
// runs that call and every other with the real git.
func (r *deployRig) gitSignalling(call string) string {
	r.t.Helper()
	wrapper := filepath.Join(r.root, "git-signalling")
	if err := os.Mkdir(wrapper, 0o755); err != nil {
		r.t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$*\" = '%s' ]; then\n  kill -INT \"$PPID\"\n  /bin/sleep 0.2\nfi\nexec '%s' \"$@\"\n", call, r.realGit)
	if err := os.WriteFile(filepath.Join(wrapper, "git"), []byte(script), 0o755); err != nil {
		r.t.Fatal(err)
	}
	return wrapper + string(os.PathListSeparator) + r.flyDir
}

func deployedLine(sha string) string { return deployedTo(sha, "colette-api-prod") }

func deployedTo(sha, app string) string {
	return "Deployed " + sha[:7] + " to " + app + ". Cadre shows it on its next check.\n"
}

// Each fault breaks one check of a clean rig.

func (r *deployRig) anotherRepo() {
	r.git(r.clone, "remote", "set-url", "origin", filepath.Join(r.root, "colette-club", "colette-web.git"))
}
func (r *deployRig) originWithNoOwner() {
	r.git(r.clone, "remote", "set-url", "origin", "https://github.com/colette-api")
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
func (r *deployRig) hiddenChange(mark string) {
	r.git(r.clone, "update-index", mark, "README.md")
	r.write(r.clone, "README.md", "changed")
}
func (r *deployRig) skipWorktreeChange()    { r.hiddenChange("--skip-worktree") }
func (r *deployRig) assumeUnchangedChange() { r.hiddenChange("--assume-unchanged") }
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
func (r *deployRig) untrackedVercel() {
	r.targets = `{"platform":"vercel","name":"web","settings":{},"tracked":false}`
}
func (r *deployRig) noAppSet() { r.targets = `{"platform":"fly","settings":{},"tracked":true}` }
func (r *deployRig) noConfigFile() {
	r.git(r.clone, "rm", "-q", "fly.toml")
	r.git(r.clone, "commit", "-m", "no config")
	r.git(r.clone, "push", "origin", "main")
}
func (r *deployRig) onlyAnIgnoredConfig() {
	r.noConfigFile()
	r.ignored("fly.toml")
}
func (r *deployRig) productionConfigDangling()     { r.danglingLink("fly.production.toml") }
func (r *deployRig) productionConfigThroughAFile() { r.linkThroughAFile("fly.production.toml") }
func (r *deployRig) onlyConfigDangling() {
	r.noConfigFile()
	r.danglingLink("fly.toml")
}
func (r *deployRig) storedConfigMissing()   { r.targets = storedTarget }
func (r *deployRig) storedConfigIsAFolder() { r.storedConfig(".") }
func (r *deployRig) flyMissing()            { r.t.Setenv("PATH", r.gitDir) }
func (r *deployRig) flySignedOut()          { r.t.Setenv("FAKE_FLY_AUTH", "1") }
func (r *deployRig) cadreFails()            { r.next = func(w http.ResponseWriter) { writeJSON(w, 500, `{}`) } }
func (r *deployRig) repoUnknown()           { r.repoGone = true }

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

// An ignored file is only on this machine, so it is passed over for the committed one.
func TestDeployPassesOverAnIgnoredProductionConfig(t *testing.T) {
	r := newDeployRig(t)
	r.ignored("fly.production.toml")

	if code := r.deploy(); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	if _, args, _ := r.flyRan(); !strings.Contains(args, " --config fly.toml ") {
		t.Errorf("fly ran with %q, want --config fly.toml", args)
	}
}

// The server accepts any path inside the repo, so a stored config may not be in git's exact form. fly gets the clean path.
func TestDeployWithAStoredConfigInAnyForm(t *testing.T) {
	for _, tc := range []struct{ stored, want string }{
		{"deploy/fly.toml", "deploy/fly.toml"},
		{"./fly.toml", "fly.toml"},
		{"deploy/../fly.toml", "fly.toml"},
		{"deploy/./fly.toml", "deploy/fly.toml"},
		{"deploy//fly.toml", "deploy/fly.toml"},
	} {
		t.Run(tc.stored, func(t *testing.T) {
			r := newDeployRig(t)
			r.pushedInFolder("deploy", "fly.toml")
			r.storedConfig(tc.stored)

			if code := r.deploy(); code != 0 {
				t.Fatalf("code = %d, stderr %q", code, r.h.errb)
			}
			if _, args, _ := r.flyRan(); !strings.Contains(args, " --config "+tc.want+" ") {
				t.Errorf("fly ran with %q, want --config %s", args, tc.want)
			}
		})
	}
}

// A stored config must be a committed file in the checkout, whatever else is on disk.
func TestDeployRefusesAStoredConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		file func(*deployRig) string
	}{
		{"that is ignored", func(r *deployRig) string {
			r.ignored("fly.custom.toml")
			return "fly.custom.toml"
		}},
		{"above the checkout", func(r *deployRig) string {
			r.write(r.root, "fly.toml", "outside")
			return "../fly.toml"
		}},
		{"above the checkout, by way of a folder", func(r *deployRig) string {
			r.write(r.root, "fly.toml", "outside")
			return "deploy/../../fly.toml"
		}},
		{"that comes to the top level itself", func(*deployRig) string { return "deploy/.." }},
		{"in another form, and not committed", func(*deployRig) string { return "./fly.custom.toml" }},
		{"at an absolute path outside the checkout", func(r *deployRig) string {
			r.write(r.root, "fly.toml", "outside")
			return filepath.Join(r.root, "fly.toml")
		}},
		{"at the absolute path of a committed file", func(r *deployRig) string {
			return filepath.Join(r.clone, "fly.toml")
		}},
		{"that is a committed folder of one file", func(r *deployRig) string {
			r.pushedInFolder("deploy", "fly.toml")
			return "deploy"
		}},
		{"that is a committed link to nowhere", func(r *deployRig) string {
			r.danglingLink("fly.custom.toml")
			return "fly.custom.toml"
		}},
		{"that is a committed link through a file", func(r *deployRig) string {
			r.linkThroughAFile("fly.custom.toml")
			return "fly.custom.toml"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDeployRig(t)
			file := tc.file(r)
			r.storedConfig(file)

			r.assertRefused(r.deploy(), refusedConfig(file))
		})
	}
}

// cadre repo set replaces the target, so the command the refusal gives must carry a stored status URL over.
func TestDeployRefusalKeepsTheStatusURL(t *testing.T) {
	const command = " in this checkout. Set the right file with cadre repo set colette-api --fly-app colette-api-prod"
	const placeholder = " --fly-config <file> --status-url <url> (see cadre repo show).\n"
	for _, tc := range []struct{ name, target, want string }{
		{
			"with a status URL",
			`{"platform":"fly","settings":{"app":"colette-api-prod"},"status_url":"https://app.test/status","tracked":true}`,
			"Neither fly.production.toml nor fly.toml is" + command + " --status-url 'https://app.test/status' --fly-config <file>.\n",
		},
		{
			"with a status URL and a stored config",
			`{"platform":"fly","settings":{"app":"colette-api-prod","config":"fly.custom.toml"},"status_url":"https://app.test/status","tracked":true}`,
			"fly.custom.toml isn't" + command + " --status-url 'https://app.test/status' --fly-config <file>.\n",
		},
		{
			"with a status URL holding an ampersand",
			`{"platform":"fly","settings":{"app":"colette-api-prod"},"status_url":"https://app.test/up?a=1&b=2%20c","tracked":true}`,
			"Neither fly.production.toml nor fly.toml is" + command + " --status-url 'https://app.test/up?a=1&b=2%20c' --fly-config <file>.\n",
		},
		{
			"with a status URL a shell would run",
			`{"platform":"fly","settings":{"app":"colette-api-prod"},"status_url":"https://app.test/up?a=$(id);b=1&c=2","tracked":true}`,
			"Neither fly.production.toml nor fly.toml is" + command + " --status-url 'https://app.test/up?a=$(id);b=1&c=2' --fly-config <file>.\n",
		},
		{
			"with a status URL holding a single quote",
			`{"platform":"fly","settings":{"app":"colette-api-prod"},"status_url":"https://app.test/up';id;'","tracked":true}`,
			"Neither fly.production.toml nor fly.toml is" + command + placeholder,
		},
		{
			"with a status URL holding a backslash before a quote",
			`{"platform":"fly","settings":{"app":"colette-api-prod"},"status_url":"https://app.test/up\\';id;'","tracked":true}`,
			"Neither fly.production.toml nor fly.toml is" + command + placeholder,
		},
		{
			"with a status URL that doesn't print",
			`{"platform":"fly","settings":{"app":"colette-api-prod"},"status_url":"https://app.test/\u001b[2J\nFake","tracked":true}`,
			"Neither fly.production.toml nor fly.toml is" + command + placeholder,
		},
		{
			"with a status URL holding a letter outside ASCII",
			`{"platform":"fly","settings":{"app":"colette-api-prod"},"status_url":"https://app.test/café","tracked":true}`,
			"Neither fly.production.toml nor fly.toml is" + command + placeholder,
		},
		{
			"with a stored config and a status URL it can't quote",
			`{"platform":"fly","settings":{"app":"colette-api-prod","config":"fly.custom.toml"},"status_url":"https://app.test/up'","tracked":true}`,
			"fly.custom.toml isn't" + command + placeholder,
		},
		{
			"with no status URL",
			`{"platform":"fly","settings":{"app":"colette-api-prod"},"status_url":null,"tracked":true}`,
			refusedNoConfig,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDeployRig(t)
			r.noConfigFile()
			r.targets = tc.target

			r.assertRefused(r.deploy(), tc.want)
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
	if strings.Contains(r.h.errb.String(), "isn't in this checkout") {
		t.Errorf("stderr = %q, want the error as it is", r.h.errb)
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

// A signal while fly runs doesn't end cadre first: it waits, and ends with fly's status. SIGTERM reaches fly only through
// cadre. SIGINT never does, as the terminal sends it to the whole group and a second one would make fly abort.
func TestDeployOutlivesASignalWhileFlyRuns(t *testing.T) {
	for _, tc := range []struct {
		signal string
		code   int
		flySaw string
	}{
		{"TERM", 7, "TERM\n"},
		{"INT", 9, ""},
	} {
		t.Run(tc.signal, func(t *testing.T) {
			r := newDeployRig(t)
			r.installFly("fly", signalledFly)
			t.Setenv("FAKE_FLY_SIGNAL", tc.signal)

			code := r.deploy()

			if code != tc.code {
				t.Errorf("code = %d, want %d", code, tc.code)
			}
			if saw, _ := os.ReadFile(r.log); string(saw) != tc.flySaw {
				t.Errorf("fly saw %q, want %q", saw, tc.flySaw)
			}
			if strings.Contains(r.h.out.String(), "Deployed") {
				t.Errorf("stdout = %q", r.h.out)
			}
		})
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

func TestDeployWithOnlyFlyctl(t *testing.T) {
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

// fly is also Concourse's CLI, so with both on the PATH that one is never called, not even to ask who is signed in.
func TestDeployPrefersFlyctl(t *testing.T) {
	r := newDeployRig(t)
	concourse := filepath.Join(r.root, "concourse.log")
	r.installFly("fly", "#!/bin/sh\necho \"$*\" >> '"+concourse+"'\n")
	r.installFly("flyctl", fakeFly)

	if code := r.deploy(); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	r.flyRan()
	if raw, err := os.ReadFile(concourse); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Concourse's fly ran: %q (%v)", raw, err)
	}
}

func TestDeployRefuses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault func(*deployRig)
		want  string
	}{
		{"a clone of another repo", (*deployRig).anotherRepo, refusedCheckout},
		{"an origin with a host and no owner", (*deployRig).originWithNoOwner, refusedCheckout},
		{"a folder outside git", (*deployRig).outsideGit, refusedCheckout},
		{"a clone with no origin", (*deployRig).noOrigin, refusedCheckout},
		{"another branch", (*deployRig).anotherBranch, refusedBranch},
		{"a detached HEAD", (*deployRig).detachedHead, refusedBranch},
		{"a staged change", (*deployRig).stagedChange, refusedClean},
		{"an unstaged change", (*deployRig).unstagedChange, refusedClean},
		{"an untracked file", (*deployRig).untrackedFile, refusedClean},
		{"a change under skip-worktree", (*deployRig).skipWorktreeChange, refusedMarked},
		{"a change under assume-unchanged", (*deployRig).assumeUnchangedChange, refusedMarked},
		{"a local commit", (*deployRig).localCommit, refusedMain},
		{"a commit pushed from a second clone", (*deployRig).commitOnOrigin, refusedMain},
		{"targets with only render", (*deployRig).onlyRender, refusedNoTarget},
		{"no targets", (*deployRig).noTargets, refusedNoTarget},
		{"a Vercel target cadre can't track", (*deployRig).untrackedVercel, refusedNoTarget},
		{"a fly target with no app", (*deployRig).noAppSet, refusedApp},
		{"a stored config the checkout doesn't have", (*deployRig).storedConfigMissing, refusedConfig("fly.custom.toml")},
		{"a stored config that is a folder", (*deployRig).storedConfigIsAFolder, refusedConfig(".")},
		{"neither config file", (*deployRig).noConfigFile, refusedNoConfig},
		{"only an ignored fly.toml", (*deployRig).onlyAnIgnoredConfig, refusedNoConfig},
		{"a committed fly.production.toml that leads nowhere", (*deployRig).productionConfigDangling, refusedConfig("fly.production.toml")},
		{"a committed fly.toml that leads nowhere", (*deployRig).onlyConfigDangling, refusedConfig("fly.toml")},
		{"a committed fly.production.toml that leads through a file", (*deployRig).productionConfigThroughAFile, refusedConfig("fly.production.toml")},
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
		{"the Fly app, then the config", (*deployRig).onlyRender, (*deployRig).noConfigFile, refusedNoTarget},
		{"the config, then fly", (*deployRig).noConfigFile, (*deployRig).flyMissing, refusedNoConfig},
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
	r.ignored("ignored.log")

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

// With main gone from origin, the origin/main left here is stale, so it can't stand for what origin has.
func TestDeployWhenMainIsGoneFromOrigin(t *testing.T) {
	r := newDeployRig(t)
	r.git(r.origin, "update-ref", "-d", "refs/heads/main")

	code := r.deploy()

	if code != 1 || !strings.HasPrefix(r.h.errb.String(), "Could not fetch origin: ") {
		t.Errorf("code = %d, stderr %q", code, r.h.errb)
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
		{"ls-files -v", "git broke\n"},
		{"fetch origin +refs/heads/main:refs/remotes/origin/main", "Could not fetch origin: git broke\n"},
		{"rev-parse HEAD", "git broke\n"},
		{"rev-parse --verify refs/remotes/origin/main", refusedMain},
		{"ls-files --full-name -- :(literal)fly.production.toml", "git broke\n"},
		{"ls-files --full-name -- :(literal)fly.toml", "git broke\n"},
	} {
		t.Run(tc.failing, func(t *testing.T) {
			r := newDeployRig(t)
			t.Setenv("PATH", r.gitFailing(tc.failing))

			r.assertRefused(r.deploy(), tc.want)
		})
	}
}

func TestDeployWhenGitFailsOnAStoredConfig(t *testing.T) {
	r := newDeployRig(t)
	r.storedConfig("fly.toml")
	t.Setenv("PATH", r.gitFailing("ls-files --full-name -- :(literal)fly.toml"))

	r.assertRefused(r.deploy(), "git broke\n")
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

// A signed-out flyctl stops the deploy: it never falls through to a fly that would run.
func TestDeployWhenFlyctlIsNotSignedIn(t *testing.T) {
	r := newDeployRig(t)
	r.installFly("fly", "#!/bin/sh\necho \"$*\" >> \"$FAKE_FLY_LOG\"\n")
	r.installFly("flyctl", fakeFly)
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

	for _, tc := range []struct {
		name          string
		stdin         io.Reader
		afterQuestion string // a read that fails or ends leaves the question's line open, so cadre ends it
	}{
		{"no", strings.NewReader("n\n"), ""},
		{"no, with a yes on the next line", strings.NewReader("n\ny\n"), ""},
		{"a bare return", strings.NewReader("\n"), ""},
		{"something else", strings.NewReader("yep\n"), ""},
		{"an empty stdin", strings.NewReader(""), "\n"},
		{"a closed stdin", iotest.ErrReader(os.ErrClosed), "\n"},
		{"a line cut short", strings.NewReader("y"), "\n"},
	} {
		t.Run(tc.name+" doesn't deploy", func(t *testing.T) {
			r := newDeployRig(t)
			r.items = openItems
			r.h.deps.Stdin = tc.stdin

			code := r.deploy()

			if code != 1 {
				t.Errorf("code = %d, want 1", code)
			}
			if r.h.errb.String() != refusedAnswer {
				t.Errorf("stderr = %q, want %q", r.h.errb, refusedAnswer)
			}
			if want := openItemsText + deployQuestion + tc.afterQuestion; r.h.out.String() != want {
				t.Errorf("stdout = %q, want %q", r.h.out, want)
			}
			r.assertNoDeploy()
		})
	}
}

// The answer takes its own line and no more: what follows is for fly, which reads the same stdin.
func TestDeployLeavesFlyTheInputAfterTheAnswer(t *testing.T) {
	r := newDeployRig(t)
	r.items = openItems
	piped, input, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { piped.Close() })
	if _, err := input.WriteString("y\nrest\n"); err != nil {
		t.Fatal(err)
	}
	input.Close()
	r.h.deps.Stdin = piped

	if code := r.deploy(); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	if _, _, stdin := r.flyRan(); stdin != "rest" {
		t.Errorf("fly read %q from stdin, want %q", stdin, "rest")
	}
}

// With --json the question is on stderr, so the line it leaves open ends there too, and stdout stays empty.
func TestDeployJSONWithNothingToRead(t *testing.T) {
	r := newDeployRig(t)
	r.items = openItems

	code := r.deploy("--json")

	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if r.h.out.Len() != 0 {
		t.Errorf("stdout = %q", r.h.out)
	}
	if want := openItemsText + deployQuestion + "\n" + refusedAnswer; r.h.errb.String() != want {
		t.Errorf("stderr = %q, want %q", r.h.errb, want)
	}
	r.assertNoDeploy()
}

// Item text comes from pull request descriptions, so it must not be able to move the cursor or forge the question.
func TestDeployPrintsAnItemOnOneLine(t *testing.T) {
	r := newDeployRig(t)
	r.items = `[{"number":7,"url":"u","text":"Run \u001b[2Kthe\r\nDeploy anyway? [y/N] y\u009b\u202Ereversed\u2028caf\u00e9"}]`

	if code := r.deploy("--yes"); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	want := "Before deploying:\n  #7 Run  [2Kthe  Deploy anyway? [y/N] y  reversed caf\u00e9\nfly output\n" + deployedLine(r.head())
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
		{"a change under skip-worktree", (*deployRig).skipWorktreeChange, refusedMarked},
		{"a change under assume-unchanged", (*deployRig).assumeUnchangedChange, refusedMarked},
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
			committed := make(chan error, 1)
			r.next = func(w http.ResponseWriter) {
				_, err := tryGit(r.clone, "commit", "--allow-empty", "-m", "local")
				committed <- err
				writeJSON(w, 200, `{"repo":"colette-api","before_deploying":`+tc.items+`}`)
			}

			code := r.deploy(tc.flags...)

			select {
			case err := <-committed:
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("Cadre was never asked what is open")
			}
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
		"ls-files -v",
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

// A repo with two Fly apps, as kinfield has. The server lists kinfield-family first, so a pick by name shows.
const (
	kinfieldTarget       = `{"platform":"fly","name":"kinfield","settings":{"app":"kinfield"},"tracked":true}`
	kinfieldFamilyTarget = `{"platform":"fly","name":"kinfield-family","settings":{"app":"kinfield-family","config":"fly.family.toml"},"tracked":true}`
	refusedSeveral       = "colette-api has 2 apps: kinfield, kinfield-family. Pass --app <name> or --all.\n"
	refusedMixed         = "colette-api has apps on more than one platform. Deploy each with --app <name>, in deploy order.\n"
	familyLeft           = "kinfield-family wasn't deployed. Deploy it with cadre deploy colette-api --app kinfield-family.\n"
)

// twoFlyApps has the server answer with kinfield-family and kinfield, and commits kinfield-family's config.
func (r *deployRig) twoFlyApps() {
	r.t.Helper()
	r.targets = kinfieldFamilyTarget + "," + kinfieldTarget
	r.pushed("fly.family.toml")
}

func TestDeployRefusesARepoWithSeveralApps(t *testing.T) {
	r := newDeployRig(t)
	r.twoFlyApps()

	r.assertRefused(r.deploy(), refusedSeveral)
}

func TestDeployOneAppOfSeveral(t *testing.T) {
	r := newDeployRig(t)
	r.twoFlyApps()
	sha := r.head()

	code := r.deploy("--app", "kinfield-family")

	if code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	if want := []string{"deploy --app kinfield-family --config fly.family.toml --image-label " + sha}; !slices.Equal(r.flyDeploys(), want) {
		t.Errorf("fly deployed %q, want %q", r.flyDeploys(), want)
	}
	if want := "fly output\n" + deployedTo(sha, "kinfield-family"); r.h.out.String() != want {
		t.Errorf("stdout = %q, want %q", r.h.out, want)
	}
}

// --app takes the app's name in cadre, which fly doesn't know: fly gets the Fly app's.
func TestDeployGoesByTheAppsNameNotFlys(t *testing.T) {
	r := newDeployRig(t)
	r.targets = `{"platform":"fly","name":"api","settings":{"app":"colette-api-prod"},"tracked":true}`
	sha := r.head()

	code := r.deploy("--app", "api")

	if code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	if _, args, _ := r.flyRan(); args != "deploy --app colette-api-prod --config fly.toml --image-label "+sha {
		t.Errorf("fly ran with %q", args)
	}
}

func TestDeployRefusesAnAppTheRepoDoesntHave(t *testing.T) {
	r := newDeployRig(t)
	r.twoFlyApps()

	r.assertRefused(r.deploy("--app", "linear"), "colette-api has no app linear. Its apps: kinfield, kinfield-family.\n")
}

func TestDeployAllDeploysEachInTurn(t *testing.T) {
	r := newDeployRig(t)
	r.twoFlyApps()
	sha := r.head()

	code := r.deploy("--all")

	if code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	want := []string{
		"deploy --app kinfield --config fly.toml --image-label " + sha,
		"deploy --app kinfield-family --config fly.family.toml --image-label " + sha,
	}
	if !slices.Equal(r.flyDeploys(), want) {
		t.Errorf("fly deployed %q, want %q", r.flyDeploys(), want)
	}
	if want := "fly output\n" + deployedTo(sha, "kinfield") + "fly output\n" + deployedTo(sha, "kinfield-family"); r.h.out.String() != want {
		t.Errorf("stdout = %q, want %q", r.h.out, want)
	}
	if want := "fly warning\nfly warning\n"; r.h.errb.String() != want {
		t.Errorf("stderr = %q, want only fly's", r.h.errb)
	}
}

// The apps after the one that failed stay as they were, and the run names them.
func TestDeployAllStopsAtTheFirstFailure(t *testing.T) {
	r := newDeployRig(t)
	r.twoFlyApps()
	t.Setenv("FAKE_FLY_EXIT", "3")
	sha := r.head()

	code := r.deploy("--all")

	if code != 3 {
		t.Errorf("code = %d, want fly's 3", code)
	}
	if want := []string{"deploy --app kinfield --config fly.toml --image-label " + sha}; !slices.Equal(r.flyDeploys(), want) {
		t.Errorf("fly deployed %q, want %q", r.flyDeploys(), want)
	}
	if r.h.out.String() != "fly output\n" {
		t.Errorf("stdout = %q, want only fly's output", r.h.out)
	}
	if want := "fly warning\n" + familyLeft; r.h.errb.String() != want {
		t.Errorf("stderr = %q, want %q", r.h.errb, want)
	}
}

// A signal while one app deploys stops the run before the next, however that deploy ended: SIGTERM, which cadre passes
// on to fly, and SIGINT, which it doesn't.
func TestDeployAllStopsAfterASignal(t *testing.T) {
	for _, sig := range []string{"INT", "TERM"} {
		t.Run(sig, func(t *testing.T) {
			r := newDeployRig(t)
			r.twoFlyApps()
			r.installFly("fly", signallingFly)
			t.Setenv("FAKE_FLY_SIGNAL", sig)
			sha := r.head()

			code := r.deploy("--all")

			if code != 1 {
				t.Errorf("code = %d, want 1", code)
			}
			if want := []string{"deploy --app kinfield --config fly.toml --image-label " + sha}; !slices.Equal(r.flyDeploys(), want) {
				t.Errorf("fly deployed %q, want %q", r.flyDeploys(), want)
			}
			if want := deployedTo(sha, "kinfield"); r.h.out.String() != want {
				t.Errorf("stdout = %q, want %q", r.h.out, want)
			}
			if r.h.errb.String() != familyLeft {
				t.Errorf("stderr = %q, want %q", r.h.errb, familyLeft)
			}
		})
	}
}

// A signal before the first app deploys, while cadre checks the checkout again, deploys nothing.
func TestDeployAllStopsOnASignalBeforeTheFirstApp(t *testing.T) {
	held := make(chan os.Signal, 1) // only so that a cadre which doesn't catch the signal can't end the test binary
	signal.Notify(held, os.Interrupt)
	t.Cleanup(func() { signal.Stop(held) })
	r := newDeployRig(t)
	r.twoFlyApps()
	r.items = openItems
	signalling := r.gitSignalling("rev-parse HEAD")
	r.h.deps.Stdin = &changingStdin{change: func() { t.Setenv("PATH", signalling) }, answer: strings.NewReader("y\n")}

	code := r.deploy("--all")

	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if r.h.errb.String() != refusedAnswer {
		t.Errorf("stderr = %q, want %q", r.h.errb, refusedAnswer)
	}
	r.assertNoDeploy()
}

// Before the first app deploys, the refusal says all there is: every app stays as it was.
func TestDeployAllRefusedBeforeTheFirstAppNamesNoApp(t *testing.T) {
	r := newDeployRig(t)
	r.twoFlyApps()
	r.items = openItems
	r.h.deps.Stdin = &changingStdin{change: r.untrackedFile, answer: strings.NewReader("y\n")}

	code := r.deploy("--all")

	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if r.h.errb.String() != refusedClean {
		t.Errorf("stderr = %q, want %q", r.h.errb, refusedClean)
	}
	r.assertNoDeploy()
}

// The apps a stopped run names come from the server, so each one's line stays one line.
func TestDeployNamesTheAppsLeftOnOneLine(t *testing.T) {
	r := newDeployRig(t)
	r.name = "colette-\u001b[2Japi\nFake: line"
	r.targets = kinfieldTarget + `,{"platform":"fly","name":"kinfield-\u001b[2Jfam\nFake: line","settings":{"app":"kinfield-family"},"tracked":true}`
	t.Setenv("FAKE_FLY_EXIT", "3")

	if code := r.deploy("--all"); code != 3 {
		t.Errorf("code = %d, want 3", code)
	}
	want := "fly warning\nkinfield- [2Jfam Fake: line wasn't deployed. Deploy it with cadre deploy colette- [2Japi Fake: line --app kinfield- [2Jfam Fake: line.\n"
	if r.h.errb.String() != want {
		t.Errorf("stderr = %q, want %q", r.h.errb, want)
	}
}

func TestDeployAllRefusesAppsOnSeveralPlatforms(t *testing.T) {
	r := newDeployRig(t)
	r.targets = kinfieldTarget + `,{"platform":"vercel","name":"web","settings":{"project":"prj_web","dir":"apps/web"},"tracked":true,"account":"colette-club"}`

	r.assertRefused(r.deploy("--all"), refusedMixed)
}

func TestDeployWithAppAndAllIsAUsageError(t *testing.T) {
	for _, flags := range [][]string{
		{"--app", "kinfield", "--all"},
		{"--all", "--app=kinfield"},
		{"--app", "--all"},
		{"--app"},
		{"--app", "kinfield", "--app", "kinfield-family"},
	} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			r := newDeployRig(t)
			r.twoFlyApps()

			code := r.deploy(flags...)

			if code != 2 {
				t.Errorf("code = %d, want 2", code)
			}
			if r.h.errb.String() != usageDeploy {
				t.Errorf("stderr = %q, want %q", r.h.errb, usageDeploy)
			}
			r.assertNoDeploy()
		})
	}
}

// Every app's config is checked before the first deploys, so a missing one deploys nothing.
func TestDeployChecksEveryConfigBeforeAnyApp(t *testing.T) {
	r := newDeployRig(t)
	r.targets = kinfieldFamilyTarget + "," + kinfieldTarget

	r.assertRefused(r.deploy("--all"),
		"fly.family.toml isn't in this checkout. Set the right file with cadre repo set colette-api --fly-app kinfield-family --fly-config <file>.\n")
}

// An older cadre names no Fly target. Its one app deploys as before, and --app finds it by its Fly app's name.
func TestDeployOneUnnamedFlyTargetNeedsNoFlag(t *testing.T) {
	for _, flags := range [][]string{nil, {"--app", "colette-api-prod"}} {
		t.Run(fmt.Sprint(flags), func(t *testing.T) {
			r := newDeployRig(t)
			r.targets = `{"platform":"fly","name":null,"settings":{"app":"colette-api-prod"},"tracked":true}`
			sha := r.head()

			code := r.deploy(flags...)

			if code != 0 {
				t.Fatalf("code = %d, stderr %q", code, r.h.errb)
			}
			if _, args, _ := r.flyRan(); args != "deploy --app colette-api-prod --config fly.toml --image-label "+sha {
				t.Errorf("fly ran with %q", args)
			}
		})
	}
}

// One question covers every app, so a yes deploys them all.
func TestDeployAllAsksOnceBeforeTheFirstApp(t *testing.T) {
	r := newDeployRig(t)
	r.twoFlyApps()
	r.items = openItems
	r.h.deps.Stdin = strings.NewReader("y\n")
	sha := r.head()

	code := r.deploy("--all")

	if code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	want := openItemsText + deployQuestion + "fly output\n" + deployedTo(sha, "kinfield") + "fly output\n" + deployedTo(sha, "kinfield-family")
	if r.h.out.String() != want {
		t.Errorf("stdout = %q, want %q", r.h.out, want)
	}
}

// fly ships the working tree, so a change made while one app deploys stops the next.
func TestDeployAllRechecksBeforeEachApp(t *testing.T) {
	r := newDeployRig(t)
	r.twoFlyApps()
	t.Setenv("FAKE_FLY_LEAVES", filepath.Join(r.clone, "left.txt"))
	sha := r.head()

	code := r.deploy("--all")

	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if want := "fly warning\n" + refusedClean + familyLeft; r.h.errb.String() != want {
		t.Errorf("stderr = %q, want %q", r.h.errb, want)
	}
	if want := []string{"deploy --app kinfield --config fly.toml --image-label " + sha}; !slices.Equal(r.flyDeploys(), want) {
		t.Errorf("fly deployed %q, want %q", r.flyDeploys(), want)
	}
}

func TestDeployAllJSONPrintsOneObjectPerApp(t *testing.T) {
	r := newDeployRig(t)
	r.twoFlyApps()
	sha := r.head()

	code := r.deploy("--all", "--json")

	if code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	want := `{"repo":"colette-api","app":"kinfield","config":"fly.toml","commit":"` + sha + `"}` + "\n" +
		`{"repo":"colette-api","app":"kinfield-family","config":"fly.family.toml","commit":"` + sha + `"}` + "\n"
	if r.h.out.String() != want {
		t.Errorf("stdout = %q, want %q", r.h.out, want)
	}
}

// cadre repo set sets the app its --name gives, else the one named after its Fly app, so the command the refusal gives
// names the app when that isn't its name.
func TestDeployRefusalNamesAnAppWhoseNameDiffers(t *testing.T) {
	const command = " in this checkout. Set the right file with cadre repo set colette-api --fly-app colette-api-prod"
	for _, tc := range []struct{ name, target, want string }{
		{
			"a name other than its Fly app's",
			`{"platform":"fly","name":"api","settings":{"app":"colette-api-prod"},"tracked":true}`,
			"Neither fly.production.toml nor fly.toml is" + command + " --name api --fly-config <file>.\n",
		},
		{
			"a name other than its Fly app's, with a status URL",
			`{"platform":"fly","name":"api","settings":{"app":"colette-api-prod"},"status_url":"https://app.test/status","tracked":true}`,
			"Neither fly.production.toml nor fly.toml is" + command + " --name api --status-url 'https://app.test/status' --fly-config <file>.\n",
		},
		{
			"its Fly app's name",
			`{"platform":"fly","name":"colette-api-prod","settings":{"app":"colette-api-prod"},"tracked":true}`,
			refusedNoConfig,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDeployRig(t)
			r.noConfigFile()
			r.targets = tc.target

			r.assertRefused(r.deploy(), tc.want)
		})
	}
}

// With no app cadre can deploy there is nothing to pick from, so a flag gets the refusal that says how to connect one.
func TestDeployWithAFlagOnARepoWithNoApp(t *testing.T) {
	for _, flags := range [][]string{{"--app", "colette-api-prod"}, {"--all"}} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			r := newDeployRig(t)
			r.onlyRender()

			r.assertRefused(r.deploy(flags...), refusedNoTarget)
		})
	}
}
