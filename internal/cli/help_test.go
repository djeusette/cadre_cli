package cli_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/djeusette/cadre_cli/internal/cli"
	"github.com/djeusette/cadre_cli/internal/config"
)

// What a wrong call of each command prints on stderr: that command's usage, and no other.
const (
	usageLogin            = "Usage: cadre login --url <url>\nRun cadre help login for details.\n"
	usageWhoami           = "Usage: cadre whoami\nRun cadre help whoami for details.\n"
	usageProjectCreate    = "Usage: cadre project create <file>\nRun cadre help project create for details.\n"
	usageProjectPull      = "Usage: cadre project pull <slug> [-o <file>]\nRun cadre help project pull for details.\n"
	usageProjectUpdate    = "Usage: cadre project update <file>\nRun cadre help project update for details.\n"
	usageProjectArchive   = "Usage: cadre project archive <slug>\nRun cadre help project archive for details.\n"
	usageProjectUnarchive = "Usage: cadre project unarchive <slug>\nRun cadre help project unarchive for details.\n"
	usageSpecPull         = "Usage: cadre spec pull <project>/<n> [-o <file>]\nRun cadre help spec pull for details.\n"
	usageSpecPush         = "Usage: cadre spec push <project>/<n> <file> [--base <version>]\nRun cadre help spec push for details.\n"
	usagePrLink           = "Usage: cadre pr link <pr-url> <project>/<n>\n       cadre pr link <pr-url> --none\nRun cadre help pr link for details.\n"
	usageRepoSet          = "Usage: cadre repo set <repo> --fly-app <app> [--fly-config <file>]\n       cadre repo set <repo> --platform <name>\nRun cadre help repo set for details.\n"
	usageRepoUnset        = "Usage: cadre repo unset <repo> --platform <name>\nRun cadre help repo unset for details.\n"
	usageRepoShow         = "Usage: cadre repo show <repo>\nRun cadre help repo show for details.\n"
	usageDeploy           = "Usage: cadre deploy <repo> [--yes]\nRun cadre help deploy for details.\n"
	usageFlyOrgAdd        = "Usage: cadre fly org add <org>\nRun cadre help fly org add for details.\n"
	usageFlyOrgRemove     = "Usage: cadre fly org remove <org>\nRun cadre help fly org remove for details.\n"
	usageUpgrade          = "Usage: cadre upgrade\nRun cadre help upgrade for details.\n"
)

// everyCommand and everyGroup are listed by hand, so a page that goes missing fails here.
var (
	everyCommand = []string{
		"login", "whoami",
		"project create", "project pull", "project update", "project archive", "project unarchive",
		"spec pull", "spec push", "pr link", "repo set", "repo unset", "repo show",
		"fly org add", "fly org remove", "deploy", "upgrade", "version", "help",
	}
	everyGroup = map[string][]string{
		"project": {"create", "pull", "update", "archive", "unarchive"},
		"spec":    {"pull", "push"},
		"pr":      {"link"},
		"repo":    {"set", "unset", "show"},
		"fly":     {"org add", "org remove"},
		"fly org": {"add", "remove"},
	}
)

// helpServer counts the calls that reach it: help never makes one.
func helpServer(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		writeJSON(w, 200, `{}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// helpFor runs args and fails unless they print a help page on stdout and nothing on stderr. It returns the page.
func helpFor(t *testing.T, srv *httptest.Server, args ...string) string {
	t.Helper()
	h := newHarness(t, srv, nil)
	if code := cli.Run(args, h.deps); code != 0 {
		t.Errorf("%q: code = %d, want 0", args, code)
	}
	if h.errb.Len() != 0 {
		t.Errorf("%q: stderr = %q", args, h.errb)
	}
	if !strings.Contains(h.out.String(), "Usage:\n") {
		t.Errorf("%q: stdout = %q, want a help page", args, h.out)
	}
	return h.out.String()
}

func TestOverviewListsEveryCommand(t *testing.T) {
	srv, _ := helpServer(t)
	overview := helpFor(t, srv, "help")

	for _, name := range everyCommand {
		if !strings.Contains(overview, "\n  "+name+"  ") {
			t.Errorf("the overview has no line for %s:\n%s", name, overview)
		}
	}
}

func TestEveryWayToAskGivesTheOverview(t *testing.T) {
	srv, _ := helpServer(t)
	want := helpFor(t, srv, "help")

	for _, args := range [][]string{nil, {"--help"}, {"-h"}, {"--json"}, {"help", "--json"}} {
		if got := helpFor(t, srv, args...); got != want {
			t.Errorf("%q printed another page:\n%s", args, got)
		}
	}
}

func TestEveryCommandHasAHelpPage(t *testing.T) {
	srv, _ := helpServer(t)
	for _, name := range everyCommand {
		words := strings.Fields(name)
		page := helpFor(t, srv, append([]string{"help"}, words...)...)
		if !strings.Contains(page, "Usage:\n  cadre "+name) {
			t.Errorf("%s: page = %q, want its usage", name, page)
		}
		for _, flag := range []string{"--help", "-h"} {
			if got := helpFor(t, srv, append(slices.Clone(words), flag)...); got != page {
				t.Errorf("%s %s printed another page:\n%s", name, flag, got)
			}
		}
	}
}

func TestEveryGroupListsItsCommands(t *testing.T) {
	srv, _ := helpServer(t)
	for group, commands := range everyGroup {
		words := strings.Fields(group)
		page := helpFor(t, srv, words...)
		for _, name := range commands {
			if !strings.Contains(page, "\n  cadre "+group+" "+name) {
				t.Errorf("%s: no usage for %s:\n%s", group, name, page)
			}
			if !strings.Contains(page, "\n  "+name+"  ") {
				t.Errorf("%s: no line for %s:\n%s", group, name, page)
			}
		}
		if got := helpFor(t, srv, append([]string{"help"}, words...)...); got != page {
			t.Errorf("help %s printed another page:\n%s", group, got)
		}
		if got := helpFor(t, srv, append(slices.Clone(words), "--help")...); got != page {
			t.Errorf("%s --help printed another page:\n%s", group, got)
		}
	}
}

// A help flag anywhere stops the command, whatever else is on the line: `--help` is never an org, a slug or a file.
func TestHelpFlagNeverRunsTheCommand(t *testing.T) {
	srv, hits := helpServer(t)
	file := writeSpec(t, "x")
	for _, args := range [][]string{
		{"project", "archive", "--help"},
		{"project", "archive", "gift-wrapping", "-h"},
		{"project", "create", file, "--help"},
		{"spec", "push", "wish-reminders/2", file, "--base", "v1", "-h"},
		{"spec", "pull", "-h", "wish-reminders/2"},
		{"pr", "link", prURL, "--none", "--help"},
		{"repo", "show", "--help"},
		{"deploy", "--help"},
		{"deploy", "colette-api", "--yes", "-h"},
		{"repo", "set", "shop-api", "--fly-app", "shop-api-prod", "-h"},
		{"fly", "org", "add", "--help"},
		{"fly", "org", "remove", "acme", "-h"},
		{"login", "--url", srv.URL, "--help"},
		{"whoami", "--json", "--help"},
		{"upgrade", "--help"},
	} {
		asked := 0
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		h.terminal("tok", nil, &asked)
		if code := cli.Run(args, h.deps); code != 0 || !strings.Contains(h.out.String(), "Usage:\n") {
			t.Errorf("%q: code = %d, stdout %q, stderr %q", args, code, h.out, h.errb)
		}
		if asked != 0 {
			t.Errorf("%q: asked for a token", args)
		}
		if len(h.opened) != 0 {
			t.Errorf("%q: opened %v", args, h.opened)
		}
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Errorf("the server was called %d times", n)
	}
}

// The help and the README say the same about which config file deploys.
func TestDeployConfigIsDocumentedInStep(t *testing.T) {
	srv, _ := helpServer(t)
	h := newHarness(t, srv, nil)
	if code := cli.Run([]string{"help", "deploy"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}

	want := "else fly.production.toml if it's committed, else fly.toml"
	for name, text := range map[string]string{"help": h.out.String(), "README": strings.ReplaceAll(string(readme), "`", "")} {
		if !strings.Contains(strings.Join(strings.Fields(text), " "), want) {
			t.Errorf("%s doesn't say %q", name, want)
		}
	}
}

func TestHelpNeedsNoConfig(t *testing.T) {
	srv, _ := helpServer(t)
	h := newHarness(t, srv, nil)
	h.deps.Getenv = func(string) string { return "" } // no XDG_CONFIG_HOME or HOME: the config can't be found

	if code := cli.Run([]string{"whoami", "--help"}, h.deps); code != 0 {
		t.Errorf("code = %d, stderr %q", code, h.errb)
	}
}

func TestHelpGivesTheNearestPageForAnUnknownSubcommand(t *testing.T) {
	srv, _ := helpServer(t)
	want := helpFor(t, srv, "spec")

	for _, args := range [][]string{{"spec", "nope", "--help"}, {"help", "spec", "nope"}} {
		if got := helpFor(t, srv, args...); got != want {
			t.Errorf("%q printed another page:\n%s", args, got)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	srv, hits := helpServer(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"rollback"}, "cadre has no rollback command.\nRun cadre help to see its commands.\n"},
		{[]string{"rollback", "shop-api", "--yes"}, "cadre has no rollback command.\nRun cadre help to see its commands.\n"},
		{[]string{"rollback", "--help"}, "cadre has no rollback command.\nRun cadre help to see its commands.\n"},
		{[]string{"help", "rollback"}, "cadre has no rollback command.\nRun cadre help to see its commands.\n"},
		{[]string{"--nope"}, "cadre has no --nope command.\nRun cadre help to see its commands.\n"},
		{[]string{"projct"}, "cadre has no projct command.\nDid you mean cadre project?\nRun cadre help to see its commands.\n"},
		{[]string{"upgrde"}, "cadre has no upgrde command.\nDid you mean cadre upgrade?\nRun cadre help to see its commands.\n"},
		{[]string{"project", "destroy", "a"}, "cadre project has no destroy command.\nRun cadre help project to see its commands.\n"},
		{[]string{"spec", "pish"}, "cadre spec has no pish command.\nDid you mean cadre spec push?\nRun cadre help spec to see its commands.\n"},
		{[]string{"pr", "unlink", prURL}, "cadre pr has no unlink command.\nDid you mean cadre pr link?\nRun cadre help pr to see its commands.\n"},
		{[]string{"repo", "nope", "colette-api"}, "cadre repo has no nope command.\nRun cadre help repo to see its commands.\n"},
		{[]string{"fly", "app", "add", "acme"}, "cadre fly has no app command.\nRun cadre help fly to see its commands.\n"},
		{[]string{"fly", "org", "list"}, "cadre fly org has no list command.\nRun cadre help fly org to see its commands.\n"},
		{[]string{"fly", "org", "ad", "acme"}, "cadre fly org has no ad command.\nDid you mean cadre fly org add?\nRun cadre help fly org to see its commands.\n"},
	} {
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		if code := cli.Run(c.args, h.deps); code != 2 {
			t.Errorf("%q: code = %d, want 2", c.args, code)
		}
		if h.errb.String() != c.want {
			t.Errorf("%q: stderr = %q, want %q", c.args, h.errb, c.want)
		}
		if h.out.Len() != 0 {
			t.Errorf("%q: stdout = %q", c.args, h.out)
		}
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Errorf("the server was called %d times", n)
	}
}

func TestWrongArgumentsPrintThatCommandsUsage(t *testing.T) {
	srv, _ := helpServer(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"login"}, usageLogin},
		{[]string{"whoami", "extra"}, usageWhoami},
		{[]string{"project", "create"}, usageProjectCreate},
		{[]string{"project", "pull"}, usageProjectPull},
		{[]string{"project", "update"}, usageProjectUpdate},
		{[]string{"project", "archive"}, usageProjectArchive},
		{[]string{"project", "unarchive"}, usageProjectUnarchive},
		{[]string{"spec", "pull"}, usageSpecPull},
		{[]string{"spec", "push"}, usageSpecPush},
		{[]string{"pr", "link"}, usagePrLink},
		{[]string{"repo", "set"}, usageRepoSet},
		{[]string{"repo", "unset"}, usageRepoUnset},
		{[]string{"repo", "show"}, usageRepoShow},
		{[]string{"deploy"}, usageDeploy},
		{[]string{"deploy", "colette-api", "shop-api"}, usageDeploy},
		{[]string{"fly", "org", "add"}, usageFlyOrgAdd},
		{[]string{"fly", "org", "remove"}, usageFlyOrgRemove},
		{[]string{"upgrade", "now"}, usageUpgrade},
	} {
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
		if code := cli.Run(c.args, h.deps); code != 2 {
			t.Errorf("%q: code = %d, want 2", c.args, code)
		}
		if h.errb.String() != c.want {
			t.Errorf("%q: stderr = %q, want %q", c.args, h.errb, c.want)
		}
		if h.out.Len() != 0 {
			t.Errorf("%q: stdout = %q", c.args, h.out)
		}
	}
}

func TestHelpFitsAnEightyColumnTerminal(t *testing.T) {
	srv, _ := helpServer(t)
	pages := [][]string{{"help"}}
	for _, name := range everyCommand {
		pages = append(pages, append([]string{"help"}, strings.Fields(name)...))
	}
	for group := range everyGroup {
		pages = append(pages, append([]string{"help"}, strings.Fields(group)...))
	}

	for _, args := range pages {
		for line := range strings.SplitSeq(helpFor(t, srv, args...), "\n") {
			if utf8.RuneCountInString(line) > 80 {
				t.Errorf("%q: %d columns: %s", args, utf8.RuneCountInString(line), line)
			}
		}
	}
}
