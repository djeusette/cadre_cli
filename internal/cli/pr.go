package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/djeusette/cadre_cli/internal/api"
)

// runPr dispatches `pr link <pr-url> (<project>/<n>|--none)`; any other form is a usage error.
func runPr(env Env, asJSON bool, args []string) int {
	if len(args) == 0 || args[0] != "link" {
		return usageError(env)
	}
	return runLink(env, asJSON, args[1:])
}

func runLink(env Env, asJSON bool, args []string) int {
	args, none := splitBoolFlag(args, "--none")
	if len(args) == 0 {
		return usageError(env)
	}
	prURL := args[0]
	milestone, rest, ok := linkTarget(args[1:], none)
	if !ok {
		return usageError(env)
	}
	link := func(ctx context.Context, s *session, _ []string) int {
		return linkPullRequest(ctx, s, prURL, milestone)
	}
	return runCommand(env, asJSON, rest, 0, link)
}

// linkTarget takes the milestone a link names from the arguments after the URL: none with --none, or else the
// <project>/<n> that comes first. ok is false when neither is given or the reference is not <project>/<n>;
// given both, the reference is left in rest, which then has one argument too many.
func linkTarget(rest []string, none bool) (*string, []string, bool) {
	if none {
		return nil, rest, true
	}
	if len(rest) == 0 {
		return nil, nil, false
	}
	if _, _, ok := parseSpecRef(rest[0]); !ok {
		return nil, nil, false
	}
	return &rest[0], rest[1:], true
}

func linkPullRequest(ctx context.Context, s *session, prURL string, milestone *string) int {
	if !s.requireKey() {
		return 1
	}

	linked, err := s.client.LinkPullRequest(ctx, prURL, milestone)
	switch {
	case errors.Is(err, api.ErrMilestoneNotFound) && milestone != nil:
		fmt.Fprintf(s.env.Stderr, "No milestone %s on %s.\n", *milestone, s.baseURL)
		return 1
	case errors.Is(err, api.ErrPullRequestNotFound):
		fmt.Fprintf(s.env.Stderr, "GitHub has no pull request at %s.\n", prURL)
		return 1
	case errors.Is(err, api.ErrGitHubNotConnected):
		fmt.Fprintf(s.env.Stderr, "GitHub isn't connected on %s yet. An org owner sets it up on its Setup page.\n", s.baseURL)
		return 1
	case errors.Is(err, api.ErrGitHubUnreachable):
		fmt.Fprintln(s.env.Stderr, "Cadre couldn't reach GitHub. Try again in a minute.")
		return 1
	case err != nil:
		return s.failAPI(err)
	}

	switch {
	case s.asJSON:
		fmt.Fprint(s.env.Stdout, string(linked.Raw))
	case linked.Milestone == nil:
		fmt.Fprintf(s.env.Stdout, "Unlinked %s #%d.\n", linked.Repo, linked.Number)
	default:
		fmt.Fprintf(s.env.Stdout, "Linked %s #%d to %s: %s\n", linked.Repo, linked.Number, *linked.Milestone, text(linked.ProjectURL))
	}
	return 0
}

// text is the string p points to, or "" when p is nil.
func text(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
