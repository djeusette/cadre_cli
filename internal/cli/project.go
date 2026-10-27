package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/djeusette/cadre_cli/internal/api"
)

// runProject dispatches `project create <file>`, `pull <slug> [-o <file>]`, `update <file>`,
// `archive <slug>` and `unarchive <slug>`; any other form is a usage error.
func runProject(env Env, asJSON bool, args []string) int {
	if len(args) == 0 {
		return usageError(env)
	}
	rest := args[1:]
	switch args[0] {
	case "create":
		return runCommand(env, asJSON, rest, 1, createProject)
	case "pull":
		output, rest, ok := takeFlag(rest, "-o")
		if !ok {
			return usageError(env)
		}
		return runCommand(env, asJSON, rest, 1, func(ctx context.Context, s *session, a []string) int {
			return pullProject(ctx, s, a[0], output)
		})
	case "update":
		return runCommand(env, asJSON, rest, 1, updateProject)
	case "archive":
		return runCommand(env, asJSON, rest, 1, func(ctx context.Context, s *session, a []string) int {
			return archiveProject(ctx, s, a[0], true)
		})
	case "unarchive":
		return runCommand(env, asJSON, rest, 1, func(ctx context.Context, s *session, a []string) int {
			return archiveProject(ctx, s, a[0], false)
		})
	default:
		return usageError(env)
	}
}

func pullProject(ctx context.Context, s *session, slug, output string) int {
	if !s.requireKey() {
		return 1
	}
	pulled, err := s.client.PullProject(ctx, slug)
	if errors.Is(err, api.ErrProjectNotFound) {
		return s.projectNotFound(slug)
	}
	if err != nil {
		return s.failAPI(err)
	}

	file, err := projectFileText(pulled.File)
	if err != nil {
		return fail(s.env, err)
	}
	if output != "" {
		if err := os.WriteFile(output, file, 0o644); err != nil {
			return fail(s.env, fmt.Errorf("cannot write %s: %w", output, err))
		}
		fmt.Fprintf(s.progress(), "Pulled %s (version %d) to %s.\n", slug, pulled.File.Version, output)
	}

	switch {
	case s.asJSON:
		fmt.Fprint(s.env.Stdout, string(pulled.Raw))
	case output == "":
		s.env.Stdout.Write(file)
	}
	return 0
}

// projectFileText is the file pull writes: 2-space indented, keys in ProjectFile's order, no HTML escaping.
func projectFileText(file api.ProjectFile) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(file); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func updateProject(ctx context.Context, s *session, args []string) int {
	body, err := os.ReadFile(args[0])
	if err != nil {
		return fail(s.env, fmt.Errorf("cannot read %s: %w", args[0], err))
	}
	var file struct {
		Slug    string `json:"slug"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal(body, &file); err != nil {
		return fail(s.env, fmt.Errorf("%s is not valid JSON", args[0]))
	}
	if file.Slug == "" {
		fmt.Fprintf(s.env.Stderr, "%s has no slug.\n", args[0])
		return 1
	}
	if !s.requireKey() {
		return 1
	}

	updated, err := s.client.UpdateProject(ctx, file.Slug, body)
	var changed *api.ProjectChangedError
	switch {
	case errors.Is(err, api.ErrProjectNotFound):
		return s.projectNotFound(file.Slug)
	case errors.Is(err, api.ErrProjectArchived):
		fmt.Fprintf(s.env.Stderr, "%s is archived. Run cadre project unarchive %s first.\n", file.Slug, file.Slug)
		return 1
	case errors.As(err, &changed):
		fmt.Fprintf(s.env.Stderr, "%s changed since you pulled it (version %d, now %d). Pull it again.\n", file.Slug, file.Version, changed.Version)
		return 1
	case err != nil:
		return s.failAPI(err)
	}

	if s.asJSON {
		fmt.Fprint(s.env.Stdout, string(updated.Raw))
		return 0
	}
	fmt.Fprintf(s.env.Stdout, "Updated %s to version %d: %s\n", updated.Slug, updated.Version, updated.URL)
	return 0
}

func archiveProject(ctx context.Context, s *session, slug string, archive bool) int {
	if !s.requireKey() {
		return 1
	}
	call := s.client.UnarchiveProject
	if archive {
		call = s.client.ArchiveProject
	}
	done, err := call(ctx, slug)
	if errors.Is(err, api.ErrProjectNotFound) {
		return s.projectNotFound(slug)
	}
	if err != nil {
		return s.failAPI(err)
	}

	switch {
	case s.asJSON:
		fmt.Fprint(s.env.Stdout, string(done.Raw))
	case archive:
		fmt.Fprintf(s.env.Stdout, "Archived %s. Run cadre project unarchive %s to bring it back.\n", done.Slug, done.Slug)
	default:
		fmt.Fprintf(s.env.Stdout, "Unarchived %s: %s\n", done.Slug, done.URL)
	}
	return 0
}

func (s *session) projectNotFound(slug string) int {
	fmt.Fprintf(s.env.Stderr, "No project %s on %s.\n", slug, s.baseURL)
	return 1
}

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
