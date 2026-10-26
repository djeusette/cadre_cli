package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/djeusette/cadre_cli/internal/api"
)

// specRef is <project>/<n>: a project slug and a milestone number from 1.
var specRef = regexp.MustCompile(`^([a-z0-9]+(?:-[a-z0-9]+)*)/([1-9][0-9]*)$`)

// runSpec dispatches `spec pull <project>/<n> [-o <file>]` and
// `spec push <project>/<n> <file> [--base <version>]`; any other form is a usage error.
func runSpec(env Env, asJSON bool, args []string) int {
	if len(args) < 2 {
		return usageError(env)
	}
	switch args[0] {
	case "pull":
		return runPull(env, asJSON, args[1:])
	case "push":
		return runPush(env, asJSON, args[1:])
	default:
		return usageError(env)
	}
}

func runPull(env Env, asJSON bool, args []string) int {
	output, rest, ok := takeFlag(args, "-o")
	if !ok || len(rest) == 0 {
		return usageError(env)
	}
	slug, number, ok := parseSpecRef(rest[0])
	if !ok {
		return usageError(env)
	}
	pull := func(ctx context.Context, s *session, _ []string) int {
		return pullSpec(ctx, s, slug, number, output)
	}
	return runCommand(env, asJSON, rest[1:], 0, pull)
}

func runPush(env Env, asJSON bool, args []string) int {
	base, rest, ok := takeFlag(args, "--base")
	if !ok || len(rest) == 0 {
		return usageError(env)
	}
	slug, number, ok := parseSpecRef(rest[0])
	if !ok {
		return usageError(env)
	}
	push := func(ctx context.Context, s *session, files []string) int {
		return pushSpec(ctx, s, slug, number, files[0], base)
	}
	return runCommand(env, asJSON, rest[1:], 1, push)
}

// takeFlag removes the flag name and its value, written `name value` or `name=value`, from args.
// The value is empty when the flag is absent; ok is false when it has no value or is given twice.
func takeFlag(args []string, name string) (string, []string, bool) {
	var value string
	found := false
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == name && i+1 < len(args) && !found:
			value, found = args[i+1], true
			i++
		case strings.HasPrefix(arg, name+"=") && len(arg) > len(name)+1 && !found:
			value, found = strings.TrimPrefix(arg, name+"="), true
		case arg == name || strings.HasPrefix(arg, name+"="):
			return "", nil, false
		default:
			rest = append(rest, arg)
		}
	}
	return value, rest, true
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

func pullSpec(ctx context.Context, s *session, slug string, number int, output string) int {
	if !s.requireKey() {
		return 1
	}

	pulled, err := s.client.PullSpec(ctx, slug, number)
	if errors.Is(err, api.ErrMilestoneNotFound) {
		fmt.Fprintf(s.env.Stderr, "No milestone %s/%d on %s.\n", slug, number, s.baseURL)
		return 1
	}
	if err != nil {
		return s.failAPI(err)
	}
	if pulled.Spec == nil {
		fmt.Fprintf(s.env.Stderr, "%s/%d has no spec yet.\n", slug, number)
		return 1
	}
	if output != "" {
		if err := os.WriteFile(output, []byte(*pulled.Spec), 0o644); err != nil {
			return fail(s.env, fmt.Errorf("cannot write %s: %w", output, err))
		}
		fmt.Fprintf(s.progress(), "Pulled the spec of %s/%d to %s.\n", slug, number, output)
	}

	switch {
	case s.asJSON:
		fmt.Fprint(s.env.Stdout, string(pulled.Raw))
	case output == "":
		fmt.Fprint(s.env.Stdout, *pulled.Spec)
	}
	return 0
}

func pushSpec(ctx context.Context, s *session, slug string, number int, file, base string) int {
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

	pushed, err := s.client.PushSpec(ctx, slug, number, string(spec), base)
	switch {
	case errors.Is(err, api.ErrMilestoneNotFound):
		fmt.Fprintf(s.env.Stderr, "No milestone %s/%d on %s.\n", slug, number, s.baseURL)
		return 1
	case errors.Is(err, api.ErrSpecChanged):
		fmt.Fprintf(s.env.Stderr, "The spec of %s/%d changed since you pulled it. Pull it again.\n", slug, number)
		return 1
	case err != nil:
		return s.failAPI(err)
	}

	if s.asJSON {
		fmt.Fprint(s.env.Stdout, string(pushed.Raw))
		return 0
	}
	fmt.Fprintf(s.env.Stdout, "Pushed the spec of %s/%d: %s\n", pushed.Slug, pushed.Milestone, pushed.URL)
	return 0
}
