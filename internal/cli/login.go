package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/colette-club/cadre/cli/internal/api"
	"github.com/colette-club/cadre/cli/internal/config"
)

const defaultPollInterval = 2 * time.Second

func login(ctx context.Context, s *session, _ []string) int {
	started, err := s.client.StartLogin(ctx)
	if err != nil {
		return s.failAPI(err)
	}

	out := s.progress()
	fmt.Fprintf(out, "Opening %s in your browser.\n", started.VerificationURI)
	fmt.Fprintf(out, "Check that the page shows this code: %s\n", started.UserCode)
	if s.env.OpenBrowser != nil {
		// The URL is already printed, so the user can open it by hand.
		_ = s.env.OpenBrowser(started.VerificationURI)
	}

	redeemed, err := pollForApproval(ctx, s, started)
	switch {
	case errors.Is(err, api.ErrExpired), errors.Is(err, api.ErrNotFound):
		fmt.Fprintln(s.env.Stderr, msgCodeExpired)
		return 1
	case err != nil:
		return s.failAPI(err)
	}

	saved := config.Config{URL: s.baseURL, APIKey: redeemed.APIKey, Email: redeemed.Email}
	if err := config.Save(s.cfgPath, saved); err != nil {
		return fail(s.env, fmt.Errorf("could not save %s: %w", s.cfgPath, err))
	}

	if s.asJSON {
		return printJSON(s, map[string]string{"email": redeemed.Email, "url": s.baseURL})
	}
	fmt.Fprintf(s.env.Stdout, "Signed in as %s.\n", redeemed.Email)
	return 0
}

// pollForApproval redeems the device code every interval until it is approved,
// gone, or expires_in seconds have passed in total (then it returns api.ErrExpired).
func pollForApproval(ctx context.Context, s *session, started api.Login) (api.Redeemed, error) {
	interval := time.Duration(started.Interval) * time.Second
	if interval <= 0 {
		interval = defaultPollInterval
	}
	polls := time.Duration(started.ExpiresIn) * time.Second / interval

	for range polls {
		s.env.Sleep(interval)
		redeemed, err := s.client.RedeemLogin(ctx, started.DeviceCode)
		if !errors.Is(err, api.ErrPending) {
			return redeemed, err
		}
	}
	return api.Redeemed{}, api.ErrExpired
}
