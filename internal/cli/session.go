package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/colette-club/cadre/cli/internal/api"
	"github.com/colette-club/cadre/cli/internal/config"
)

const (
	msgNotSignedIn = "Not signed in. Run cadre login."
	msgKeyInvalid  = "Your key is no longer valid. Run cadre login."
	msgCodeExpired = "The code expired. Run cadre login again."
)

// session is what a command needs once the config is loaded.
type session struct {
	env     Env
	asJSON  bool
	cfgPath string
	cfg     config.Config
	baseURL string
	client  *api.Client
}

type command func(ctx context.Context, s *session, args []string) int

// runCommand checks the argument count, loads the config and runs cmd.
func runCommand(env Env, asJSON bool, args []string, wantArgs int, cmd command) int {
	if len(args) != wantArgs {
		return usageError(env)
	}

	cfgPath, err := config.Path(env.Getenv)
	if err != nil {
		return fail(env, err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fail(env, err)
	}

	baseURL := config.BaseURL(cfg, env.Getenv)
	s := &session{
		env:     env,
		asJSON:  asJSON,
		cfgPath: cfgPath,
		cfg:     cfg,
		baseURL: baseURL,
		client: &api.Client{
			BaseURL:   baseURL,
			APIKey:    cfg.APIKey,
			UserAgent: "cadre-cli/" + env.Version,
			HTTP:      env.HTTP,
		},
	}
	return cmd(context.Background(), s, args)
}

// progress is where status lines go: stdout, or stderr when stdout carries JSON.
func (s *session) progress() io.Writer {
	if s.asJSON {
		return s.env.Stderr
	}
	return s.env.Stdout
}

// requireKey reports whether the config holds an API key, and says so if not.
func (s *session) requireKey() bool {
	if s.cfg.APIKey == "" {
		fmt.Fprintln(s.env.Stderr, msgNotSignedIn)
		return false
	}
	return true
}

// failAPI prints a failed API call to stderr and returns exit code 1.
func (s *session) failAPI(err error) int {
	var verr *api.ValidationError
	switch {
	case errors.Is(err, api.ErrUnauthorized):
		fmt.Fprintln(s.env.Stderr, msgKeyInvalid)
	case errors.As(err, &verr):
		for _, fe := range verr.Errors {
			fmt.Fprintf(s.env.Stderr, "%s: %s\n", fe.Path, fe.Message)
		}
	default:
		fmt.Fprintf(s.env.Stderr, "Could not complete the request to %s: %v\n", s.baseURL, err)
	}
	return 1
}

func fail(env Env, err error) int {
	fmt.Fprintln(env.Stderr, err)
	return 1
}
