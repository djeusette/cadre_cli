package cli

import (
	"context"
	"encoding/json"
	"fmt"
)

func whoami(ctx context.Context, s *session, _ []string) int {
	if !s.requireKey() {
		return 1
	}

	me, err := s.client.Me(ctx)
	if err != nil {
		return s.failAPI(err)
	}

	if s.asJSON {
		return printJSON(s, map[string]string{"email": me.Email, "url": s.baseURL})
	}
	fmt.Fprintf(s.env.Stdout, "Signed in to %s as %s.\n", s.baseURL, me.Email)
	return 0
}

func printJSON(s *session, v any) int {
	data, err := json.Marshal(v)
	if err != nil {
		return fail(s.env, err)
	}
	fmt.Fprintln(s.env.Stdout, string(data))
	return 0
}
