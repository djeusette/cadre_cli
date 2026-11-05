package git_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/djeusette/cadre_cli/internal/git"
)

func TestParseRepository(t *testing.T) {
	cases := map[string]string{
		"git@github.com:colette-club/colette-api.git":             "colette-club/colette-api",
		"git@github-work:colette-club/colette-api.git":            "colette-club/colette-api",
		"ssh://git@github.com/colette-club/colette-api.git":       "colette-club/colette-api",
		"https://github.com/colette-club/colette-api":             "colette-club/colette-api",
		"https://github.com/colette-club/colette-api.git/":        "colette-club/colette-api",
		"https://x-token@github.com/Colette-Club/Colette-API.git": "Colette-Club/Colette-API",
		"/tmp/x/colette-club/colette-api.git":                     "colette-club/colette-api",
		"":                                                        "",
		"colette-api":                                             "",
	}
	for remote, want := range cases {
		got, ok := git.ParseRepository(remote)
		if got != want || ok != (want != "") {
			t.Errorf("ParseRepository(%q) = (%q, %v), want (%q, %v)", remote, got, ok, want, want != "")
		}
	}
}

// run runs git in dir and fails the test on error.
func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// commit adds a file and commits it.
func commit(t *testing.T, dir, name string) {
	t.Helper()
	write(t, filepath.Join(dir, name), name)
	run(t, dir, "add", name)
	run(t, dir, "commit", "-m", name)
}

func gitEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
}

// repo makes a repository with one commit on main and no remote.
func repo(t *testing.T) string {
	t.Helper()
	gitEnv(t)
	dir := t.TempDir()
	run(t, dir, "init", "-b", "main")
	commit(t, dir, "first.txt")
	return dir
}

// clone makes an origin with one commit and a clone of it, and returns both.
func clone(t *testing.T) (origin, work string) {
	t.Helper()
	gitEnv(t)
	root := t.TempDir()
	origin = filepath.Join(root, "colette-club", "colette-api.git")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, origin, "init", "--bare", "-b", "main")
	seed := filepath.Join(root, "seed")
	run(t, root, "clone", origin, seed)
	run(t, seed, "checkout", "-b", "main")
	commit(t, seed, "first.txt")
	run(t, seed, "push", "origin", "main")
	work = filepath.Join(root, "work")
	run(t, root, "clone", origin, work)
	return origin, work
}

func open(t *testing.T, dir string) git.Checkout {
	t.Helper()
	t.Chdir(dir)
	c, err := git.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return c
}

func TestOpen(t *testing.T) {
	dir := repo(t)
	sub := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	c := open(t, sub)
	want, _ := filepath.EvalSymlinks(dir)
	got, _ := filepath.EvalSymlinks(c.Dir)
	if got != want {
		t.Errorf("Dir = %q, want %q", got, want)
	}
}

func TestOpenOutsideACheckout(t *testing.T) {
	gitEnv(t)
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(t.TempDir()))
	t.Chdir(t.TempDir())
	if _, err := git.Open(context.Background()); !errors.Is(err, git.ErrNotACheckout) {
		t.Errorf("err = %v, want ErrNotACheckout", err)
	}
}

func TestRepository(t *testing.T) {
	_, work := clone(t)
	got, err := open(t, work).Repository(context.Background())
	if err != nil || got != "colette-club/colette-api" {
		t.Errorf("Repository = (%q, %v)", got, err)
	}
}

func TestRepositoryWithoutOrigin(t *testing.T) {
	if _, err := open(t, repo(t)).Repository(context.Background()); !errors.Is(err, git.ErrNotACheckout) {
		t.Errorf("err = %v, want ErrNotACheckout", err)
	}
}

func TestBranch(t *testing.T) {
	dir := repo(t)
	c := open(t, dir)
	if got, err := c.Branch(context.Background()); err != nil || got != "main" {
		t.Errorf("Branch = (%q, %v)", got, err)
	}
	run(t, dir, "checkout", "--detach")
	if got, err := c.Branch(context.Background()); err != nil || got != "" {
		t.Errorf("detached Branch = (%q, %v)", got, err)
	}
}

func TestClean(t *testing.T) {
	dir := repo(t)
	write(t, filepath.Join(dir, ".gitignore"), "ignored.txt\n")
	run(t, dir, "add", ".gitignore")
	run(t, dir, "commit", "-m", "ignore")
	c := open(t, dir)

	check := func(name string, want bool) {
		t.Helper()
		got, err := c.Clean(context.Background())
		if err != nil || got != want {
			t.Errorf("%s: Clean = (%v, %v), want %v", name, got, err, want)
		}
	}

	check("fresh", true)
	write(t, filepath.Join(dir, "ignored.txt"), "x")
	check("ignored file", true)

	write(t, filepath.Join(dir, "staged.txt"), "x")
	run(t, dir, "add", "staged.txt")
	check("staged", false)
	run(t, dir, "reset", "--hard")

	write(t, filepath.Join(dir, "first.txt"), "changed")
	check("unstaged", false)
	run(t, dir, "checkout", "first.txt")

	write(t, filepath.Join(dir, "untracked.txt"), "x")
	check("untracked", false)
	run(t, dir, "config", "status.showUntrackedFiles", "no")
	check("untracked, showUntrackedFiles no", false)
}

func TestHeadAndOriginMain(t *testing.T) {
	_, work := clone(t)
	c := open(t, work)
	ctx := context.Background()
	head, err := c.Head(ctx)
	if err != nil || len(head) != 40 {
		t.Fatalf("Head = (%q, %v)", head, err)
	}
	if om, err := c.OriginMain(ctx); err != nil || om != head {
		t.Errorf("OriginMain = (%q, %v), want %q", om, err, head)
	}
	commit(t, work, "local.txt")
	newHead, _ := c.Head(ctx)
	if om, _ := c.OriginMain(ctx); om == newHead {
		t.Errorf("OriginMain should differ after a local commit")
	}
}

func TestFetch(t *testing.T) {
	origin, work := clone(t)
	c := open(t, work)
	ctx := context.Background()
	before, _ := c.OriginMain(ctx)

	other := filepath.Join(t.TempDir(), "other")
	run(t, filepath.Dir(other), "clone", origin, other)
	commit(t, other, "second.txt")
	run(t, other, "push", "origin", "main")

	if err := c.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if after, _ := c.OriginMain(ctx); after == before {
		t.Errorf("OriginMain did not move after Fetch")
	}

	if err := os.RemoveAll(origin); err != nil {
		t.Fatal(err)
	}
	err := c.Fetch(ctx)
	if err == nil || strings.TrimSpace(err.Error()) == "" {
		t.Errorf("Fetch without origin = %v, want git's message", err)
	}
}
