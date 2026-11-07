package cli_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/djeusette/cadre_cli/internal/cli"
)

// fakeVercel stands in for vercel. Only whoami answers without logging: signed out unless cadre passes
// --non-interactive, and asleep for $FAKE_VERCEL_WHOAMI_SLEEP seconds when that is set, as a device-code login waits.
// Every other call adds "<folder> <args>" to calls.log in $FAKE_VERCEL_DIR, and a "read" line when stdin gave it one.
// A build waits, up to two seconds, until all $FAKE_VERCEL_APPS builds have started, so builds run one after the other
// fail. Like vercel 58 with --json --non-interactive, it prints its progress on stderr, other https URLs among it, and
// on stdout only a pretty-printed JSON payload, whose deployment.url is the URL. The env vars name the app, by its folder's name, that fails each way.
const fakeVercel = `#!/bin/sh
log="$FAKE_VERCEL_DIR/calls.log"
app=${PWD##*/}
started() { set -- "$FAKE_VERCEL_DIR"/started.*; echo $#; }
if [ "$1" = whoami ]; then
  [ "$*" = "whoami --non-interactive" ] || exit 97
  [ -n "$FAKE_VERCEL_WHOAMI_SLEEP" ] && exec /bin/sleep "$FAKE_VERCEL_WHOAMI_SLEEP"
  echo ada
  exit "$FAKE_VERCEL_WHOAMI"
fi
echo "$PWD $*" >> "$log"
if read -r line; then echo "$PWD read $line" >> "$log"; fi
case "$1" in
deploy)
  : > "$FAKE_VERCEL_DIR/started.$app"
  i=0
  while [ "$(started)" -lt "$FAKE_VERCEL_APPS" ]; do
    i=$((i + 1))
    if [ "$i" -gt 200 ]; then echo "$app built alone" >&2; exit 3; fi
    /bin/sleep 0.01
  done
  if [ "$app" = "$FAKE_VERCEL_BROKEN" ]; then
    echo "Building $app" >&2
    echo "Error: $app's build broke" >&2
    exit 1
  fi
  if [ "$app" = "$FAKE_VERCEL_NO_URL" ]; then
    echo '{'
    echo '  "status": "ok",'
    echo '  "deployment": {'
    echo "    \"id\": \"dpl_$app\""
    echo '  }'
    echo '}'
    exit 0
  fi
  echo "Building $app" >&2
  echo "Inspect: https://vercel.com/colette/$app/dpl_$app" >&2
  echo "Production: https://$app-stderr.vercel.app" >&2
  status=ok target=production
  [ "$app" = "$FAKE_VERCEL_ERRORED" ] && status=error
  [ "$app" = "$FAKE_VERCEL_PREVIEW" ] && target=preview
  echo '{'
  echo "  \"status\": \"$status\","
  echo '  "deployment": {'
  echo "    \"id\": \"dpl_$app\","
  echo "    \"url\": \"https://$app-1a2b.vercel.app\","
  echo "    \"inspectorUrl\": \"https://vercel.com/colette/$app/dpl_$app\","
  echo '    "readyState": "READY",'
  echo "    \"target\": \"$target\""
  echo '  },'
  echo "  \"message\": \"Deployment $app-1a2b.vercel.app ready.\","
  echo '  "next": ['
  echo "    { \"command\": \"vercel curl https://$app-next.vercel.app\", \"when\": \"Verify deployment\" }"
  echo '  ]'
  echo '}'
  echo "$PWD built" >> "$log"
  ;;
promote)
  if [ "$app" = "$FAKE_VERCEL_INTERRUPTER" ]; then
    trap 'echo "$PWD TERM" >> "$log"; exit 7' TERM
    kill -"$FAKE_VERCEL_PROMOTE_SIGNAL" "$PPID"
    /bin/sleep 0.2
  fi
  if [ "$app" = "$FAKE_VERCEL_UNPROMOTABLE" ]; then echo "Error: $app can't be promoted" >&2; exit 1; fi
  echo "Success! $app is live"
  ;;
*)
  exit 99
  ;;
esac
`

// signalledVercel builds like fakeVercel, except that once every build has started, the one in $FAKE_VERCEL_SIGNALLER
// sends $FAKE_VERCEL_SIGNAL to cadre, its parent. A signal a build gets shows in calls.log and ends it. A build that
// gets none prints its JSON, with its URL, after half a second.
const signalledVercel = `#!/bin/sh
log="$FAKE_VERCEL_DIR/calls.log"
app=${PWD##*/}
started() { set -- "$FAKE_VERCEL_DIR"/started.*; echo $#; }
[ "$1" = whoami ] && exit 0
echo "$PWD $*" >> "$log"
[ "$1" = promote ] && exit 0
trap 'echo "$PWD TERM" >> "$log"; exit 7' TERM
trap 'echo "$PWD INT" >> "$log"; exit 8' INT
: > "$FAKE_VERCEL_DIR/started.$app"
i=0
while [ "$(started)" -lt "$FAKE_VERCEL_APPS" ] && [ "$i" -lt 200 ]; do i=$((i + 1)); /bin/sleep 0.01; done
[ "$app" = "$FAKE_VERCEL_SIGNALLER" ] && kill -"$FAKE_VERCEL_SIGNAL" "$PPID"
for i in 1 2 3 4 5 6 7 8 9 10; do /bin/sleep 0.05; done
echo "{\"status\": \"ok\", \"deployment\": {\"url\": \"https://$app-1a2b.vercel.app\", \"target\": \"production\"}}"
`

const (
	refusedVercel       = "vercel isn't installed. See https://vercel.com/docs/cli.\n"
	refusedVercelSignIn = "vercel isn't signed in. Run vercel login.\n"
	refusedMixed        = "colette-api has apps on Fly and Vercel. cadre deploy can't deploy it yet.\n"
	unknownCommit       = "0123456789abcdef0123456789abcdef01234567"
)

// rigApp is one Vercel target the rig's server answers with. An empty team goes out as null, and last is the
// last_deploy value as JSON.
type rigApp struct {
	name, project, dir, team, last string
}

func (a rigApp) json() string {
	team := "null"
	if a.team != "" {
		team = jsonString(a.team)
	}
	return fmt.Sprintf(`{"platform":"vercel","name":%s,"settings":{"project":%s,"dir":%s},"tracked":true,"account":%s,"last_deploy":%s}`,
		jsonString(a.name), jsonString(a.project), jsonString(a.dir), team, a.last)
}

func jsonString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// lastDeploy is a last_deploy on commit.
func lastDeploy(commit string) string {
	return `{"commit":"` + commit + `","deployed_at":"2026-11-06T10:00:00Z"}`
}

const lastDeployNoCommit = `{"commit":null,"deployed_at":"2026-11-06T10:00:00Z"}`

// vercelRig is deployRig with a fake vercel beside the fake fly, and a server that answers with two Vercel apps: web
// in apps/web, which it lists first, and admin in apps/admin, each with a committed .vercelignore.
type vercelRig struct {
	*deployRig
	dir  string // the fake vercel's calls.log and markers
	apps []rigApp
}

func newVercelRig(t *testing.T) *vercelRig {
	t.Helper()
	r := &vercelRig{deployRig: newDeployRig(t)}
	r.dir = filepath.Join(r.root, "vercel-calls")
	if err := os.Mkdir(r.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r.installFly("vercel", fakeVercel)
	r.apps = []rigApp{
		{name: "web", project: "prj_web", dir: "apps/web", team: "colette-club", last: "null"},
		{name: "admin", project: "prj_admin", dir: "apps/admin", team: "colette-ops", last: "null"},
	}
	r.withVercelignore("apps/web")
	r.withVercelignore("apps/admin")
	for key, value := range map[string]string{
		"FAKE_VERCEL_DIR":            r.dir,
		"FAKE_VERCEL_WHOAMI":         "0",
		"FAKE_VERCEL_WHOAMI_SLEEP":   "",
		"FAKE_VERCEL_APPS":           "2",
		"FAKE_VERCEL_BROKEN":         "",
		"FAKE_VERCEL_NO_URL":         "",
		"FAKE_VERCEL_ERRORED":        "",
		"FAKE_VERCEL_PREVIEW":        "",
		"FAKE_VERCEL_UNPROMOTABLE":   "",
		"FAKE_VERCEL_INTERRUPTER":    "",
		"FAKE_VERCEL_PROMOTE_SIGNAL": "INT",
	} {
		t.Setenv(key, value)
	}
	return r
}

// withVercelignore commits and pushes a .vercelignore in folder.
func (r *vercelRig) withVercelignore(folder string) {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Join(r.clone, folder), 0o755); err != nil {
		r.t.Fatal(err)
	}
	r.pushed(filepath.Join(folder, ".vercelignore"))
}

func (r *vercelRig) app(name string) *rigApp {
	r.t.Helper()
	i := slices.IndexFunc(r.apps, func(a rigApp) bool { return a.name == name })
	if i < 0 {
		r.t.Fatalf("no app %s", name)
	}
	return &r.apps[i]
}

func (r *vercelRig) deploy(flags ...string) int {
	r.t.Helper()
	targets := make([]string, len(r.apps))
	for i, a := range r.apps {
		targets[i] = a.json()
	}
	r.targets = strings.Join(targets, ",")
	return r.deployRig.deploy(flags...)
}

// calls is calls.log's lines, or none when vercel never ran.
func (r *vercelRig) calls() []string {
	r.t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.dir, "calls.log"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		r.t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

func (r *vercelRig) promotions() []string {
	var promoted []string
	for _, call := range r.calls() {
		if strings.Contains(call, " promote ") {
			promoted = append(promoted, call)
		}
	}
	return promoted
}

func (r *vercelRig) assertNothingRan() {
	r.t.Helper()
	if calls := r.calls(); len(calls) != 0 {
		r.t.Errorf("vercel ran: %q", calls)
	}
	r.assertNoDeploy()
}

func (r *vercelRig) assertVercelRefused(code int, want string) {
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
	r.assertNothingRan()
}

func (r *vercelRig) folder(dir string) string { return filepath.Join(r.clone, dir) }

func buildCall(folder, project, team string) string {
	return folder + " deploy --prod --skip-domain --yes --non-interactive --json --project " + project + " --scope " + team
}

func promoteCall(folder, app, team string) string {
	return folder + " promote https://" + app + "-1a2b.vercel.app --yes --non-interactive --scope " + team
}

// noURLPayload is what the fake vercel prints on stdout for a build whose JSON has no deployment.url.
func noURLPayload(app string) string {
	return "{\n  \"status\": \"ok\",\n  \"deployment\": {\n    \"id\": \"dpl_" + app + "\"\n  }\n}"
}

func vercelDeployedLine(sha string, apps string) string {
	return "Deployed " + sha[:7] + " to " + apps + ". Cadre shows each on its next check.\n"
}

func buildingLine(sha string, apps string) string {
	return "Building " + sha[:7] + " for " + apps + ".\n"
}

func promotingLine(apps string) string { return "Promoting " + apps + ".\n" }

// vercelDeployedOutput is the progress of a deploy that went live: the builds, the promotions, then where it went.
func vercelDeployedOutput(sha string, apps string) string {
	return buildingLine(sha, apps) + promotingLine(apps) + vercelDeployedLine(sha, apps)
}

// pushedElsewhere commits on a new branch from HEAD, pushes it, and comes back, so the checkout knows a commit HEAD
// doesn't contain. It returns that commit.
func (r *vercelRig) pushedElsewhere(branch string) string {
	r.t.Helper()
	r.git(r.clone, "switch", "-c", branch)
	r.commit(r.clone, branch+".txt")
	r.git(r.clone, "push", "origin", branch)
	sha := r.head()
	r.git(r.clone, "switch", "-")
	return sha
}

func lacksLine(app, sha string) string {
	return "This commit lacks what " + app + " runs now (" + sha[:min(len(sha), 7)] + ").\n"
}

func TestVercelDeployBuildsEveryAppThenPromotes(t *testing.T) {
	r := newVercelRig(t)
	r.h.deps.Stdin = strings.NewReader("for vercel\n")
	sha := r.head()
	admin, web := r.folder("apps/admin"), r.folder("apps/web")

	code := r.deploy()

	if code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	calls := r.calls()
	if len(calls) != 6 {
		t.Fatalf("calls = %q, want two builds, then two promotions", calls)
	}
	builds := slices.Sorted(slices.Values(calls[:4]))
	want := []string{buildCall(admin, "prj_admin", "colette-ops"), admin + " built", buildCall(web, "prj_web", "colette-club"), web + " built"}
	if slices.Sort(want); !slices.Equal(builds, want) {
		t.Errorf("builds = %q, want %q", builds, want)
	}
	if want := []string{promoteCall(admin, "admin", "colette-ops"), promoteCall(web, "web", "colette-club")}; !slices.Equal(calls[4:], want) {
		t.Errorf("promotions = %q, want %q", calls[4:], want)
	}
	if r.h.out.String() != vercelDeployedOutput(sha, "admin, web") {
		t.Errorf("stdout = %q, want %q", r.h.out, vercelDeployedOutput(sha, "admin, web"))
	}
	if r.h.errb.Len() != 0 {
		t.Errorf("stderr = %q, want none of vercel's output", r.h.errb)
	}
	r.assertNoDeploy()
}

// An app at the repo's root builds at the top level, wherever in the checkout cadre runs.
func TestVercelDeployBuildsARootAppAtTheTopLevel(t *testing.T) {
	r := newVercelRig(t)
	r.apps = []rigApp{{name: "site", project: "prj_site", dir: ".", team: "colette-club", last: "null"}}
	r.pushed(".vercelignore")
	t.Setenv("FAKE_VERCEL_APPS", "1")
	sha := r.head()
	t.Chdir(r.folder("apps/web"))

	if code := r.deploy(); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	want := []string{
		buildCall(r.clone, "prj_site", "colette-club"),
		r.clone + " built",
		promoteCall(r.clone, "colette-api", "colette-club"),
	}
	if calls := r.calls(); !slices.Equal(calls, want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
	if r.h.out.String() != vercelDeployedOutput(sha, "site") {
		t.Errorf("stdout = %q", r.h.out)
	}
}

func TestVercelDeployFromABranchOtherThanMain(t *testing.T) {
	r := newVercelRig(t)
	r.git(r.clone, "switch", "-c", "feature")
	r.commit(r.clone, "feature.txt")
	r.git(r.clone, "push", "origin", "feature")
	sha := r.head()

	if code := r.deploy(); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	if r.h.out.String() != vercelDeployedOutput(sha, "admin, web") {
		t.Errorf("stdout = %q", r.h.out)
	}
	if len(r.promotions()) != 2 {
		t.Errorf("calls = %q", r.calls())
	}
}

func TestVercelDeployRefusesACommitNotOnOrigin(t *testing.T) {
	for name, fault := range map[string]func(*vercelRig){
		"a local commit":        func(r *vercelRig) { r.localCommit() },
		"a branch never pushed": func(r *vercelRig) { r.git(r.clone, "switch", "-c", "feature"); r.commit(r.clone, "feature.txt") },
		"a commit gone from origin": func(r *vercelRig) {
			r.git(r.clone, "switch", "-c", "feature")
			r.commit(r.clone, "feature.txt")
			r.git(r.clone, "push", "origin", "feature")
			r.git(r.clone, "push", "origin", "--delete", "feature")
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newVercelRig(t)
			fault(r)

			r.assertVercelRefused(r.deploy(), r.head()[:7]+" isn't on origin. Push it, then deploy again.\n")
		})
	}
}

func TestVercelDeployRefusesAnUncleanTree(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault func(*deployRig)
		want  string
	}{
		{"a staged change", (*deployRig).stagedChange, refusedClean},
		{"an unstaged change", (*deployRig).unstagedChange, refusedClean},
		{"an untracked file", (*deployRig).untrackedFile, refusedClean},
		{"an untracked file in an app's folder", func(r *deployRig) { r.write(r.clone, "apps/web/.env", "SECRET=1") }, refusedClean},
		{"a change under skip-worktree", (*deployRig).skipWorktreeChange, refusedMarked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newVercelRig(t)
			tc.fault(r.deployRig)

			r.assertVercelRefused(r.deploy(), tc.want)
		})
	}
}

func TestVercelDeployRefusesAMissingFolder(t *testing.T) {
	for _, tc := range []struct{ name, dir string }{
		{"a folder the checkout doesn't have", "apps/gone"},
		{"a folder above the checkout", "../colette-api"},
		{"an absolute folder", "/tmp"},
		{"a file", "README.md"},
		{"no folder", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newVercelRig(t)
			r.app("admin").dir = tc.dir

			r.assertVercelRefused(r.deploy(), tc.dir+" isn't in this checkout.\n")
		})
	}
}

func TestVercelDeployRefusesAFolderWithNoVercelignore(t *testing.T) {
	for name, fault := range map[string]func(*vercelRig){
		"no file":               func(*vercelRig) {},
		"a folder in its place": func(r *vercelRig) { r.pushedInFolder("apps/admin/.vercelignore", "keep") },
	} {
		t.Run(name, func(t *testing.T) {
			r := newVercelRig(t)
			r.pushed("apps/admin/package.json")
			r.git(r.clone, "rm", "-q", "apps/admin/.vercelignore")
			r.git(r.clone, "commit", "-m", "no .vercelignore")
			r.git(r.clone, "push", "origin", "main")
			fault(r)

			r.assertVercelRefused(r.deploy(), "apps/admin has no .vercelignore. Add one that lets through only what admin's build reads.\n")
		})
	}
}

func TestVercelDeployNamesTheRootWhenItHasNoVercelignore(t *testing.T) {
	for _, dir := range []string{".", "./"} {
		t.Run(dir, func(t *testing.T) {
			r := newVercelRig(t)
			r.apps = []rigApp{{name: "site", project: "prj_site", dir: dir, team: "colette-club", last: "null"}}

			r.assertVercelRefused(r.deploy(), "The repo's root has no .vercelignore. Add one that lets through only what site's build reads.\n")
		})
	}
}

func TestVercelDeployRefusesAnAppWithNoTeam(t *testing.T) {
	r := newVercelRig(t)
	r.app("admin").team = ""

	r.assertVercelRefused(r.deploy(), "admin has no Vercel team in cadre. Connect it again with cadre repo set colette-api --vercel-project prj_admin --name admin --vercel-dir apps/admin\n")
}

// Each app is checked in full, in name order, whatever order the server lists them in.
func TestVercelDeployChecksInOrder(t *testing.T) {
	noTeam := func(app string) func(*vercelRig) { return func(r *vercelRig) { r.app(app).team = "" } }
	noFolder := func(app string) func(*vercelRig) { return func(r *vercelRig) { r.app(app).dir = "apps/gone" } }
	for _, tc := range []struct {
		name          string
		first, second func(*vercelRig)
		want          string
	}{
		{"the checkout, then clean", func(r *vercelRig) { r.anotherRepo() }, func(r *vercelRig) { r.untrackedFile() }, refusedCheckout},
		{"clean, then the fetch", func(r *vercelRig) { r.untrackedFile() }, func(r *vercelRig) { r.originGone() }, refusedClean},
		{"the fetch, then origin", func(r *vercelRig) { r.originGone() }, func(r *vercelRig) { r.localCommit() }, "Could not fetch origin: "},
		{"origin, then the apps", func(r *vercelRig) { r.localCommit() }, noTeam("admin"), " isn't on origin. Push it, then deploy again.\n"},
		{"an app's team, then its folder", noTeam("admin"), noFolder("admin"), "admin has no Vercel team in cadre."},
		{"an app before the next by name", noFolder("admin"), noTeam("web"), "apps/gone isn't in this checkout.\n"},
		{"the apps, then vercel", noTeam("web"), func(r *vercelRig) { r.vercelMissing() }, "web has no Vercel team in cadre."},
		{"vercel, then signed in", func(r *vercelRig) { r.vercelMissing() }, func(r *vercelRig) { r.vercelSignedOut() }, refusedVercel},
		{"signed in, then the open items", func(r *vercelRig) { r.vercelSignedOut() }, func(r *vercelRig) { r.cadreFails() }, refusedVercelSignIn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newVercelRig(t)
			tc.first(r)
			tc.second(r)

			code := r.deploy()

			if code != 1 || !strings.Contains(r.h.errb.String(), tc.want) {
				t.Errorf("code = %d, stderr %q, want %q", code, r.h.errb, tc.want)
			}
			r.assertNothingRan()
		})
	}
}

func (r *vercelRig) vercelMissing() {
	if err := os.Remove(filepath.Join(r.flyDir, "vercel")); err != nil {
		r.t.Fatal(err)
	}
}
func (r *vercelRig) vercelSignedOut() { r.t.Setenv("FAKE_VERCEL_WHOAMI", "1") }

func TestVercelDeployAsksWhenTheCommitLacksWhatAnAppRuns(t *testing.T) {
	r := newVercelRig(t)
	live := r.pushedElsewhere("production")
	r.app("web").last = lastDeploy(live)

	code := r.deploy()

	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if want := lacksLine("web", live) + deployQuestion + "\n"; r.h.out.String() != want {
		t.Errorf("stdout = %q, want %q", r.h.out, want)
	}
	if r.h.errb.String() != refusedAnswer {
		t.Errorf("stderr = %q, want %q", r.h.errb, refusedAnswer)
	}
	r.assertNothingRan()
}

func TestVercelDeployGoesOnWhenTheAnswerIsYes(t *testing.T) {
	r := newVercelRig(t)
	webLive, adminLive := r.pushedElsewhere("production"), r.pushedElsewhere("hotfix")
	r.app("web").last = lastDeploy(webLive)
	r.app("admin").last = lastDeploy(adminLive)
	r.h.deps.Stdin = strings.NewReader("y\n")
	sha := r.head()

	if code := r.deploy(); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	want := lacksLine("admin", adminLive) + lacksLine("web", webLive) + deployQuestion + vercelDeployedOutput(sha, "admin, web")
	if r.h.out.String() != want {
		t.Errorf("stdout = %q, want %q", r.h.out, want)
	}
	if len(r.promotions()) != 2 {
		t.Errorf("calls = %q", r.calls())
	}
}

// A commit the checkout doesn't know, or that isn't a full SHA, can't be shown to be in this one. git never sees one
// that isn't, as it could read it as a ref or an option.
func TestVercelDeployCountsAnUnknownLiveCommitAsMissing(t *testing.T) {
	for name, live := range map[string]func(*vercelRig) string{
		"an unknown commit":           func(*vercelRig) string { return unknownCommit },
		"an abbreviated known commit": func(r *vercelRig) string { return r.head()[:12] },
		"a ref":                       func(*vercelRig) string { return "HEAD" },
		"an option":                   func(*vercelRig) string { return "--all" },
	} {
		t.Run(name, func(t *testing.T) {
			r := newVercelRig(t)
			commit := live(r)
			r.app("admin").last = lastDeploy(commit)

			code := r.deploy()

			if code != 1 {
				t.Errorf("code = %d, want 1", code)
			}
			if want := lacksLine("admin", commit) + deployQuestion + "\n"; r.h.out.String() != want {
				t.Errorf("stdout = %q, want %q", r.h.out, want)
			}
			r.assertNothingRan()
		})
	}
}

// cadre's commits are 40-hex SHA-1, so a 64-hex name counts as missing, and git never sees it: here, git would break.
func TestVercelDeployCountsA64HexLiveCommitAsMissing(t *testing.T) {
	r := newVercelRig(t)
	live := strings.Repeat("ab", 32)
	r.app("admin").last = lastDeploy(live)
	t.Setenv("PATH", r.gitBreaking("merge-base --is-ancestor "+live+" "+r.head()))

	code := r.deploy()

	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if want := lacksLine("admin", live) + deployQuestion + "\n"; r.h.out.String() != want {
		t.Errorf("stdout = %q, want %q", r.h.out, want)
	}
	r.assertNothingRan()
}

func TestVercelDeployDoesntAskWhenTheCommitContainsWhatsLive(t *testing.T) {
	for _, tc := range []struct {
		name string
		last func(*vercelRig) (web, admin string)
	}{
		{"an ancestor and HEAD itself", func(r *vercelRig) (string, string) {
			first := r.git(r.clone, "rev-list", "--max-parents=0", "HEAD")
			return lastDeploy(first), lastDeploy(r.head())
		}},
		{"an uppercase SHA", func(r *vercelRig) (string, string) { return lastDeploy(strings.ToUpper(r.head())), "null" }},
		{"no last deploy, or one with no commit", func(*vercelRig) (string, string) { return "null", lastDeployNoCommit }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newVercelRig(t)
			r.app("web").last, r.app("admin").last = tc.last(r)
			sha := r.head()

			if code := r.deploy(); code != 0 {
				t.Fatalf("code = %d, stderr %q", code, r.h.errb)
			}
			if r.h.out.String() != vercelDeployedOutput(sha, "admin, web") {
				t.Errorf("stdout = %q, want only the deployed line", r.h.out)
			}
		})
	}
}

func TestVercelDeployAsksOnceForItemsAndMissingCommits(t *testing.T) {
	r := newVercelRig(t)
	live := r.pushedElsewhere("production")
	r.app("web").last = lastDeploy(live)
	r.items = openItems
	r.h.deps.Stdin = strings.NewReader("y\n")
	sha := r.head()

	if code := r.deploy(); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	if want := openItemsText + lacksLine("web", live) + deployQuestion + vercelDeployedOutput(sha, "admin, web"); r.h.out.String() != want {
		t.Errorf("stdout = %q, want %q", r.h.out, want)
	}
}

func TestVercelDeployYesGoesOnWithoutAsking(t *testing.T) {
	r := newVercelRig(t)
	live := r.pushedElsewhere("production")
	r.app("web").last = lastDeploy(live)
	r.items = openItems
	sha := r.head()

	if code := r.deploy("--yes"); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	if want := openItemsText + lacksLine("web", live) + vercelDeployedOutput(sha, "admin, web"); r.h.out.String() != want {
		t.Errorf("stdout = %q, want %q", r.h.out, want)
	}
	if len(r.promotions()) != 2 {
		t.Errorf("calls = %q", r.calls())
	}
}

// The question can wait for minutes, and vercel uploads the working tree.
func TestVercelDeployRechecksBeforeBuilding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*vercelRig)
		want   func(sha string) string
	}{
		{"a commit", func(r *vercelRig) { r.localCommit() }, func(sha string) string { return "HEAD is no longer at " + sha[:7] + ". Deploy again.\n" }},
		{"a switch to another pushed commit", func(r *vercelRig) { r.git(r.clone, "switch", "--detach", "HEAD~1") }, func(sha string) string { return "HEAD is no longer at " + sha[:7] + ". Deploy again.\n" }},
		{"an untracked file", func(r *vercelRig) { r.write(r.clone, "apps/web/.env", "SECRET=1") }, func(string) string { return refusedClean }},
		{"an edit", func(r *vercelRig) { r.unstagedChange() }, func(string) string { return refusedClean }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newVercelRig(t)
			r.items = openItems
			r.h.deps.Stdin = &changingStdin{change: func() { tc.change(r) }, answer: strings.NewReader("y\n")}
			sha := r.head()

			code := r.deploy()

			if code != 1 || r.h.errb.String() != tc.want(sha) {
				t.Errorf("code = %d, stderr %q, want %q", code, r.h.errb, tc.want(sha))
			}
			r.assertNothingRan()
		})
	}
}

func TestVercelDeployWhenCadreCantAnswer(t *testing.T) {
	r := newVercelRig(t)
	r.cadreFails()

	code := r.deploy("--yes")

	if code != 1 || r.h.errb.Len() == 0 {
		t.Errorf("code = %d, stderr %q", code, r.h.errb)
	}
	r.assertNothingRan()
}

func TestVercelDeployWhenGitFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failing func(r *vercelRig) string
		want    string
	}{
		{"the fetch", func(*vercelRig) string { return "fetch origin --prune" }, "Could not fetch origin: git broke\n"},
		{"asking origin", func(r *vercelRig) string {
			return "for-each-ref --contains " + r.head() + " --format=%(refname) refs/remotes/origin/"
		}, "git broke\n"},
		{"comparing the live commit", func(r *vercelRig) string {
			live := r.pushedElsewhere("production")
			r.app("web").last = lastDeploy(live)
			return "merge-base --is-ancestor " + live + " " + r.head()
		}, "git broke\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newVercelRig(t)
			t.Setenv("PATH", r.gitBreaking(tc.failing(r)))

			r.assertVercelRefused(r.deploy(), tc.want)
		})
	}
}

// gitBreaking is gitFailing with git exiting 2, as merge-base exiting 1 only means "not an ancestor".
func (r *vercelRig) gitBreaking(call string) string {
	r.t.Helper()
	path := r.gitFailing(call)
	wrapper := filepath.Join(r.root, "git-wrapper", "git")
	script, err := os.ReadFile(wrapper)
	if err != nil {
		r.t.Fatal(err)
	}
	broken := strings.Replace(string(script), "  exit 1\n", "  exit 2\n", 1)
	if err := os.WriteFile(wrapper, []byte(broken), 0o755); err != nil {
		r.t.Fatal(err)
	}
	return path
}

func TestVercelDeployPromotesNothingWhenABuildFails(t *testing.T) {
	for _, tc := range []struct {
		name, broken, noURL string
		want                string
	}{
		{"one fails", "web", "", "web's build failed:\nBuilding web\nError: web's build broke\n"},
		{"one's JSON has no deployment.url", "", "admin", "admin's build failed:\n" + noURLPayload("admin") + "\nvercel gave no production deployment to promote.\n"},
		{"both fail", "web", "admin", "admin's build failed:\n" + noURLPayload("admin") + "\nvercel gave no production deployment to promote.\nweb's build failed:\nBuilding web\nError: web's build broke\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newVercelRig(t)
			t.Setenv("FAKE_VERCEL_BROKEN", tc.broken)
			t.Setenv("FAKE_VERCEL_NO_URL", tc.noURL)

			code := r.deploy()

			if code != 1 {
				t.Errorf("code = %d, want 1", code)
			}
			if want := tc.want + refusedAnswer; r.h.errb.String() != want {
				t.Errorf("stderr = %q, want %q", r.h.errb, want)
			}
			if want := buildingLine(r.head(), "admin, web"); r.h.out.String() != want {
				t.Errorf("stdout = %q, want %q", r.h.out, want)
			}
			if promoted := r.promotions(); len(promoted) != 0 {
				t.Errorf("promoted %q", promoted)
			}
		})
	}
}

// A build whose JSON doesn't say ok, or says it built a preview, gave no production deployment to promote.
func TestVercelDeployPromotesNothingWithoutAnOkProductionBuild(t *testing.T) {
	for name, env := range map[string]string{"a status other than ok": "FAKE_VERCEL_ERRORED", "a preview build": "FAKE_VERCEL_PREVIEW"} {
		t.Run(name, func(t *testing.T) {
			r := newVercelRig(t)
			t.Setenv(env, "admin")

			code := r.deploy()

			if code != 1 {
				t.Errorf("code = %d, want 1", code)
			}
			stderr := r.h.errb.String()
			if !strings.HasPrefix(stderr, "admin's build failed:\n") || !strings.Contains(stderr, `"url": "https://admin-1a2b.vercel.app"`) ||
				!strings.HasSuffix(stderr, "\nvercel gave no production deployment to promote.\n"+refusedAnswer) {
				t.Errorf("stderr = %q, want admin's build failed, with what vercel printed", stderr)
			}
			if promoted := r.promotions(); len(promoted) != 0 {
				t.Errorf("promoted %q", promoted)
			}
		})
	}
}

func TestVercelDeployWhenVercelCantStart(t *testing.T) {
	r := newVercelRig(t)
	t.Setenv("FAKE_VERCEL_APPS", "1")
	r.installFly("vercel", "#!/bin/sh\n[ \"$1\" = whoami ] && /bin/rm \"$0\"\nexit 0\n") // gone once cadre builds

	code := r.deploy()

	if code != 1 || !strings.HasPrefix(r.h.errb.String(), "admin's build failed:\nCould not run vercel: ") {
		t.Errorf("code = %d, stderr %q", code, r.h.errb)
	}
	if !strings.HasSuffix(r.h.errb.String(), "\n"+refusedAnswer) {
		t.Errorf("stderr = %q, want it to end with %q", r.h.errb, refusedAnswer)
	}
}

// The first promotion that fails stops the ones after it, so those apps stay as they were, and cadre says where every
// app stands. Running it again builds them all and promotes them all.
func TestVercelDeployNamesTheAppThatDidntGoLive(t *testing.T) {
	for _, tc := range []struct {
		unpromotable string
		tried        []string                // the apps whose promotion ran, in order
		standing     func(sha string) string // where the apps stand
	}{
		{"admin", []string{"admin"}, func(string) string { return "admin, web weren't promoted.\n" }},
		{"web", []string{"admin", "web"}, func(sha string) string { return "admin went live on " + sha[:7] + ". web wasn't promoted.\n" }},
	} {
		t.Run(tc.unpromotable, func(t *testing.T) {
			r := newVercelRig(t)
			t.Setenv("FAKE_VERCEL_UNPROMOTABLE", tc.unpromotable)
			sha := r.head()

			code := r.deploy()

			if code != 1 {
				t.Errorf("code = %d, want 1", code)
			}
			want := tc.unpromotable + " built but didn't go live: Error: " + tc.unpromotable + " can't be promoted\n" + tc.standing(sha)
			if r.h.errb.String() != want {
				t.Errorf("stderr = %q, want %q", r.h.errb, want)
			}
			if want := buildingLine(sha, "admin, web") + promotingLine("admin, web"); r.h.out.String() != want {
				t.Errorf("stdout = %q, want %q", r.h.out, want)
			}
			teams := map[string]string{"admin": "colette-ops", "web": "colette-club"}
			var tried []string
			for _, app := range tc.tried {
				tried = append(tried, promoteCall(r.folder("apps/"+app), app, teams[app]))
			}
			if promoted := r.promotions(); !slices.Equal(promoted, tried) {
				t.Errorf("promoted %q, want %q", promoted, tried)
			}
		})
	}
}

// vercel prints its progress, other https URLs among it, on stderr, and the JSON on stdout. Only the JSON's
// deployment.url goes to vercel promote.
func TestVercelDeployPromotesTheURLFromVercelsJSON(t *testing.T) {
	r := newVercelRig(t)

	if code := r.deploy(); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	want := []string{promoteCall(r.folder("apps/admin"), "admin", "colette-ops"), promoteCall(r.folder("apps/web"), "web", "colette-club")}
	if promoted := r.promotions(); !slices.Equal(promoted, want) {
		t.Errorf("promoted %q, want %q", promoted, want)
	}
	if r.h.errb.Len() != 0 {
		t.Errorf("stderr = %q, want none of vercel's progress", r.h.errb)
	}
}

// SIGTERM reaches every build only through cadre. SIGINT never does, as the terminal sends it to the whole group. Either
// way, nothing goes live, even a build that ends well, and cadre says only that: a build the signal ended didn't fail.
func TestVercelDeployOnASignalPromotesNothing(t *testing.T) {
	for _, tc := range []struct {
		signal  string
		saw     []string // what each build saw
		stderr  string
		comment string
	}{
		{"TERM", []string{"TERM"}, refusedAnswer, "both builds end on the TERM"},
		{"INT", nil, refusedAnswer, "both builds end well"},
	} {
		t.Run(tc.signal, func(t *testing.T) {
			r := newVercelRig(t)
			r.installFly("vercel", signalledVercel)
			t.Setenv("FAKE_VERCEL_SIGNALLER", "web")
			t.Setenv("FAKE_VERCEL_SIGNAL", tc.signal)

			code := r.deploy()

			if code != 1 {
				t.Errorf("code = %d, want 1", code)
			}
			if r.h.errb.String() != tc.stderr {
				t.Errorf("stderr = %q, want %q (%s)", r.h.errb, tc.stderr, tc.comment)
			}
			var saw []string
			for _, call := range r.calls() {
				if strings.HasSuffix(call, " TERM") || strings.HasSuffix(call, " INT") {
					saw = append(saw, call)
				}
			}
			var want []string
			for _, sig := range tc.saw {
				want = append(want, r.folder("apps/admin")+" "+sig, r.folder("apps/web")+" "+sig)
			}
			if slices.Sort(saw); !slices.Equal(saw, want) {
				t.Errorf("builds saw %q, want %q", saw, want)
			}
			if promoted := r.promotions(); len(promoted) != 0 {
				t.Errorf("promoted %q", promoted)
			}
			if strings.Contains(r.h.out.String(), "Deployed") {
				t.Errorf("stdout = %q", r.h.out)
			}
		})
	}
}

// A signal during a promotion stops the ones not started yet. The promotion it came during may have gone through, so
// cadre says to check, whether vercel ended on the signal (TERM, which cadre passes on) or went on (INT).
func TestVercelDeployOnASignalWhilePromoting(t *testing.T) {
	for _, tc := range []struct {
		signal, interrupter string
		tried               []string // the apps whose promotion ran, in order
		stderr              string
	}{
		{"INT", "admin", []string{"admin"},
			"cadre stopped while promoting admin. Check Vercel to see whether it went live.\nweb built but didn't go live: cadre stopped before promoting it.\n"},
		{"TERM", "admin", []string{"admin"},
			"cadre stopped while promoting admin. Check Vercel to see whether it went live.\nweb built but didn't go live: cadre stopped before promoting it.\n"},
		{"TERM", "web", []string{"admin", "web"}, "cadre stopped while promoting web. Check Vercel to see whether it went live.\n"},
	} {
		t.Run(tc.signal+" while promoting "+tc.interrupter, func(t *testing.T) {
			r := newVercelRig(t)
			t.Setenv("FAKE_VERCEL_INTERRUPTER", tc.interrupter)
			t.Setenv("FAKE_VERCEL_PROMOTE_SIGNAL", tc.signal)

			code := r.deploy()

			if code != 1 {
				t.Errorf("code = %d, want 1", code)
			}
			if r.h.errb.String() != tc.stderr {
				t.Errorf("stderr = %q, want %q", r.h.errb, tc.stderr)
			}
			teams := map[string]string{"admin": "colette-ops", "web": "colette-club"}
			var tried []string
			for _, app := range tc.tried {
				tried = append(tried, promoteCall(r.folder("apps/"+app), app, teams[app]))
			}
			if promoted := r.promotions(); !slices.Equal(promoted, tried) {
				t.Errorf("promoted %q, want %q", promoted, tried)
			}
			if strings.Contains(r.h.out.String(), "Deployed") {
				t.Errorf("stdout = %q", r.h.out)
			}
		})
	}
}

func TestVercelDeployWithoutVercelOrSignedOut(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault func(*vercelRig)
		want  string
	}{
		{"vercel missing", (*vercelRig).vercelMissing, refusedVercel},
		{"vercel signed out", (*vercelRig).vercelSignedOut, refusedVercelSignIn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newVercelRig(t)
			tc.fault(r)

			r.assertVercelRefused(r.deploy(), tc.want)
		})
	}
}

// Signed out under Claude Code, vercel whoami starts a device-code login and waits. cadre gives up on it as signed out.
func TestVercelDeployGivesUpOnAWhoamiThatWaits(t *testing.T) {
	r := newVercelRig(t)
	t.Setenv("FAKE_VERCEL_WHOAMI_SLEEP", "10")
	cli.SetVercelWhoamiTimeout(t, 200*time.Millisecond)
	start := time.Now()

	code := r.deploy()

	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %v, want cadre to stop waiting", took)
	}
	r.assertVercelRefused(code, refusedVercelSignIn)
}

func TestDeployRefusesAppsOnFlyAndVercel(t *testing.T) {
	r := newVercelRig(t)

	code := r.deployWithTargets(flyTarget + "," + r.apps[0].json())

	r.assertVercelRefused(code, refusedMixed)
}

// deployWithTargets deploys with the server answering targets as they are.
func (r *vercelRig) deployWithTargets(targets string) int {
	r.t.Helper()
	r.targets = targets
	return r.deployRig.deploy()
}

// Targets cadre can't track don't make a repo mixed: only Fly and Vercel are tracked.
func TestDeployIgnoresUntrackedTargetsWhenPicking(t *testing.T) {
	r := newVercelRig(t)
	r.apps = r.apps[:1]
	t.Setenv("FAKE_VERCEL_APPS", "1")

	code := r.deployWithTargets(renderTarget + "," + r.apps[0].json())

	if code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	if len(r.promotions()) != 1 {
		t.Errorf("calls = %q", r.calls())
	}
}

func TestVercelDeployJSONPrintsOneObjectPerApp(t *testing.T) {
	r := newVercelRig(t)
	sha := r.head()

	code := r.deploy("--json")

	if code != 0 {
		t.Fatalf("code = %d, stderr %q", code, r.h.errb)
	}
	type deployed struct{ Repo, App, Commit, URL string }
	var got []deployed
	for line := range strings.Lines(r.h.out.String()) {
		var d deployed
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			t.Fatalf("stdout line %q: %v", line, err)
		}
		got = append(got, d)
	}
	want := []deployed{
		{"colette-api", "admin", sha, "https://admin-1a2b.vercel.app"},
		{"colette-api", "web", sha, "https://web-1a2b.vercel.app"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("stdout = %q, want %v", r.h.out, want)
	}
	wantKeys := `{"repo":"colette-api","app":"admin","commit":"` + sha + `","url":"https://admin-1a2b.vercel.app"}` + "\n"
	if !strings.HasPrefix(r.h.out.String(), wantKeys) {
		t.Errorf("stdout = %q, want it to start with %q", r.h.out, wantKeys)
	}
	if r.h.errb.String() != vercelDeployedOutput(sha, "admin, web") {
		t.Errorf("stderr = %q", r.h.errb)
	}
}

// An app's name comes from the server, so it never reaches the terminal as anything but one line of text.
func TestVercelDeployPrintsAnAppNameOnOneLine(t *testing.T) {
	r := newVercelRig(t)
	r.app("admin").team = ""
	r.app("admin").name = "ad\u001b[2Kmin\nDeploy anyway? [y/N] y"

	code := r.deploy()

	if code != 1 || strings.ContainsAny(strings.TrimSuffix(r.h.errb.String(), "\n"), "\n\u001b") {
		t.Errorf("code = %d, stderr %q", code, r.h.errb)
	}
	r.assertNothingRan()
}
