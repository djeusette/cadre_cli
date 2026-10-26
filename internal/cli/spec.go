package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"unicode/utf8"

	"github.com/djeusette/cadre_cli/internal/api"
)

// specRef is <project>/<n>: a project slug and a milestone number from 1.
var specRef = regexp.MustCompile(`^([a-z0-9]+(?:-[a-z0-9]+)*)/([1-9][0-9]*)$`)

// runSpec dispatches `spec push <project>/<n> <file>`; any other form is a usage error.
func runSpec(env Env, asJSON bool, args []string) int {
	if len(args) < 2 || args[0] != "push" {
		return usageError(env)
	}
	slug, number, ok := parseSpecRef(args[1])
	if !ok {
		return usageError(env)
	}
	push := func(ctx context.Context, s *session, rest []string) int {
		return pushSpec(ctx, s, slug, number, rest[0])
	}
	return runCommand(env, asJSON, args[2:], 1, push)
}

// parseSpecRef splits <project>/<n>, refusing anything else, including a number too large for an int.
func parseSpecRef(ref string) (string, int, bool) {
	m := specRef.FindStringSubmatch(ref)
	if m == nil {
		return "", 0, false
	}
	number, err := strconv.Atoi(m[2])
	if err != nil {
		return "", 0, false
	}
	return m[1], number, true
}

func pushSpec(ctx context.Context, s *session, slug string, number int, file string) int {
	spec, err := os.ReadFile(file)
	if err != nil {
		return fail(s.env, fmt.Errorf("cannot read %s: %w", file, err))
	}
	if !utf8.Valid(spec) {
		fmt.Fprintf(s.env.Stderr, "%s is not UTF-8 text.\n", file)
		return 1
	}
	if !s.requireKey() {
		return 1
	}

	pushed, err := s.client.PushSpec(ctx, slug, number, string(spec))
	if errors.Is(err, api.ErrMilestoneNotFound) {
		fmt.Fprintf(s.env.Stderr, "No milestone %s/%d on %s.\n", slug, number, s.baseURL)
		return 1
	}
	if err != nil {
		return s.failAPI(err)
	}

	if s.asJSON {
		fmt.Fprint(s.env.Stdout, string(pushed.Raw))
		return 0
	}
	fmt.Fprintf(s.env.Stdout, "Pushed the spec of %s/%d: %s\n", pushed.Slug, pushed.Milestone, pushed.URL)
	return 0
}
