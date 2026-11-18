package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/djeusette/cadre_cli/internal/api"
	"github.com/djeusette/cadre_cli/internal/git"
)

// runCheckoutLink is `cadre link`: it tells cadre where the person keeps the repo of the checkout it runs in, so the
// repo's Deploy button opens Claude Desktop there. It sends the main checkout, even from a linked worktree, and says
// when that checkout's AGENTS.md has no Skills table.
func runCheckoutLink(ctx context.Context, s *session, _ []string) int {
	if !s.requireKey() {
		return 1
	}

	repository, path, err := mainCheckout(ctx)
	if errors.Is(err, git.ErrNotACheckout) {
		fmt.Fprintln(s.env.Stderr, "This folder isn't a checkout with an origin. Run cadre link in one.")
		return 1
	}
	if err != nil {
		return fail(s.env, err)
	}

	linked, err := s.client.LinkCheckout(ctx, repository, path)
	switch {
	case errors.Is(err, api.ErrRepositoryNotFound):
		fmt.Fprintf(s.env.Stderr, "cadre doesn't know %s. A project names a repo before cadre knows it.\n", repository)
		return 1
	case err != nil:
		return s.failAPI(err)
	}

	if s.asJSON {
		fmt.Fprint(s.env.Stdout, string(linked.Raw))
		return 0
	}
	fmt.Fprintf(s.env.Stdout, "Linked %s to %s. Its Deploy button in cadre opens there.\n", linked.Repository, linked.Path)
	if lacksSkillsTable(path) {
		fmt.Fprintf(s.env.Stdout, "%s has no Skills table in its AGENTS.md. Setup shows how to write one: %s/setup?part=skills\n",
			linked.Repository, s.baseURL)
	}
	return 0
}

// skillsSection is the line that starts a repo's Skills table, as cadre reads it from GitHub.
var skillsSection = regexp.MustCompile(`(?m)^## Skills[ \t]*\r?$`)

// lacksSkillsTable is whether the checkout at dir has no AGENTS.md, or one with no Skills section. An AGENTS.md it
// can't read says nothing either way, so it is not reported.
func lacksSkillsTable(dir string) bool {
	text, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return true
	case err != nil:
		return false
	}
	return !skillsSection.Match(text)
}

// mainCheckout is the working directory's origin, as "owner/name", and the top level of its main checkout.
func mainCheckout(ctx context.Context) (repository, path string, err error) {
	checkout, err := git.Open(ctx)
	if err != nil {
		return "", "", err
	}
	if repository, err = checkout.Repository(ctx); err != nil {
		return "", "", err
	}
	if path, err = checkout.Main(ctx); err != nil {
		return "", "", err
	}
	return repository, path, nil
}
