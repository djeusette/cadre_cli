package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

func createProject(ctx context.Context, s *session, args []string) int {
	body, err := os.ReadFile(args[0])
	if err != nil {
		return fail(s.env, fmt.Errorf("cannot read %s: %w", args[0], err))
	}
	if !json.Valid(body) {
		return fail(s.env, fmt.Errorf("%s is not valid JSON", args[0]))
	}
	if !s.requireKey() {
		return 1
	}

	created, err := s.client.CreateProject(ctx, body)
	if err != nil {
		return s.failAPI(err)
	}

	if s.asJSON {
		fmt.Fprint(s.env.Stdout, string(created.Raw))
		return 0
	}
	fmt.Fprintf(s.env.Stdout, "Registered %s with %d milestones: %s\n", created.Slug, created.Milestones, created.URL)
	return 0
}
