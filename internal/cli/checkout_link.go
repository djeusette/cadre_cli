package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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
		fmt.Fprintf(s.env.Stdout, "No Skills table in %s. Setup shows how to write one: %s/setup?part=skills\n",
			filepath.Join(linked.Path, "AGENTS.md"), s.baseURL)
	}
	return 0
}

// maxAgentsMD is how much of AGENTS.md is read: GitHub's contents API, which cadre reads it through, gives no more.
const maxAgentsMD = 1 << 20

// lacksSkillsTable is whether the checkout at dir has no AGENTS.md, or one with no Skills section. An AGENTS.md that
// isn't a regular file, or that it can't read, says nothing either way, so it is not reported.
func lacksSkillsTable(dir string) bool {
	file := filepath.Join(dir, "AGENTS.md")
	info, err := os.Stat(file)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return true
	case err != nil, !info.Mode().IsRegular():
		return false
	}
	f, err := os.Open(file)
	if err != nil {
		return false
	}
	defer f.Close()
	text, err := io.ReadAll(io.LimitReader(f, maxAgentsMD))
	if err != nil {
		return false
	}
	return !hasSkillsSection(string(text))
}

// hasSkillsSection is whether text has a line that is exactly "## Skills", trailing spaces allowed, outside fenced
// code blocks, as cadre and the cadre skill read a repo's Skills table.
func hasSkillsSection(text string) bool {
	fenced := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, " \t\r")
		switch {
		case strings.HasPrefix(line, "```"):
			fenced = !fenced
		case !fenced && line == "## Skills":
			return true
		}
	}
	return false
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
