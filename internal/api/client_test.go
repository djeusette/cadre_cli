package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/djeusette/cadre_cli/internal/api"
)

func newClient(t *testing.T, handler http.HandlerFunc) *api.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &api.Client{BaseURL: srv.URL, APIKey: "cadre_secret", UserAgent: "cadre-cli/test", HTTP: srv.Client()}
}

func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

func rateLimited(retryAfter string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"rate_limited"}`)
	}
}

func TestEveryCallMapsTooManyRequests(t *testing.T) {
	ctx := context.Background()
	calls := map[string]func(*api.Client) error{
		"start login":    func(c *api.Client) error { _, err := c.StartLogin(ctx); return err },
		"redeem login":   func(c *api.Client) error { _, err := c.RedeemLogin(ctx, "dev"); return err },
		"me":             func(c *api.Client) error { _, err := c.Me(ctx); return err },
		"create project": func(c *api.Client) error { _, err := c.CreateProject(ctx, []byte(`{}`)); return err },
		"push spec":      func(c *api.Client) error { _, err := c.PushSpec(ctx, "wish-reminders", 2, "x"); return err },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			c := newClient(t, rateLimited("30"))
			var limited *api.RateLimitedError
			if err := call(c); !errors.As(err, &limited) || limited.RetryAfter != 30*time.Second {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestTooManyRequestsWithoutUsableRetryAfter(t *testing.T) {
	for _, header := range []string{"", "soon", "-5", "Wed, 21 Oct 2026 07:28:00 GMT"} {
		c := newClient(t, rateLimited(header))
		_, err := c.Me(context.Background())
		var limited *api.RateLimitedError
		if !errors.As(err, &limited) || limited.RetryAfter != 0 {
			t.Errorf("Retry-After %q: err = %v", header, err)
		}
	}
}

func TestTooManyRequestsCapsRetryAfterAtADay(t *testing.T) {
	c := newClient(t, rateLimited("99999999999999"))
	_, err := c.Me(context.Background())
	var limited *api.RateLimitedError
	if !errors.As(err, &limited) || limited.RetryAfter != 24*time.Hour {
		t.Fatalf("err = %v", err)
	}
}

func TestRedeemLoginStatuses(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr error
	}{
		{"pending", 202, `{"status":"pending"}`, api.ErrPending},
		{"expired", 410, `{"error":"expired"}`, api.ErrExpired},
		{"not found", 404, `{"error":"not_found"}`, api.ErrNotFound},
		{"approved", 200, `{"api_key":"k","email":"ada@colette.club","expires_at":"x"}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClient(t, reply(tt.status, tt.body))
			got, err := c.RedeemLogin(context.Background(), "dev")
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && (got.APIKey != "k" || got.Email != "ada@colette.club") {
				t.Errorf("got %+v", got)
			}
		})
	}
}

func TestClientSendsHeadersAndBody(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Header.Get("Authorization") != "Bearer cadre_secret" ||
			r.Header.Get("Content-Type") != "application/json" ||
			r.Header.Get("User-Agent") != "cadre-cli/test" ||
			r.URL.Path != "/api/projects" || string(b) != `{"a":1}` {
			t.Errorf("bad request: %v %s %s", r.Header, r.URL.Path, b)
		}
		w.WriteHeader(201)
		io.WriteString(w, `{"slug":"s","milestones":2,"url":"u"}`)
	})
	got, err := c.CreateProject(context.Background(), []byte(`{"a":1}`))
	if err != nil || got.Slug != "s" || got.Milestones != 2 || got.URL != "u" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if string(got.Raw) != `{"slug":"s","milestones":2,"url":"u"}` {
		t.Errorf("raw = %s", got.Raw)
	}
}

func TestCreateProjectValidation(t *testing.T) {
	c := newClient(t, reply(422, `{"errors":[{"path":"name","message":"is required"},{"path":"slug","message":"taken"}]}`))
	_, err := c.CreateProject(context.Background(), []byte(`{}`))
	var verr *api.ValidationError
	if !errors.As(err, &verr) || len(verr.Errors) != 2 || verr.Errors[1] != (api.FieldError{Path: "slug", Message: "taken"}) {
		t.Fatalf("err = %v", err)
	}
}

func TestRedeemLoginRejectsEmptyKeyOrEmail(t *testing.T) {
	for _, body := range []string{`{"api_key":"","email":"a@b.c"}`, `{"api_key":"k","email":""}`, `{}`} {
		c := newClient(t, reply(200, body))
		if got, err := c.RedeemLogin(context.Background(), "dev"); err == nil || got != (api.Redeemed{}) {
			t.Errorf("body %s: got %+v, %v", body, got, err)
		}
	}
}

func TestNonJSONSuccessNamesServerAndStatus(t *testing.T) {
	c := newClient(t, reply(200, `<html>proxy</html>`))
	_, err := c.Me(context.Background())
	var syn *json.SyntaxError
	if err == nil || !errors.As(err, &syn) ||
		!strings.Contains(err.Error(), "unexpected response from "+c.BaseURL+" (status 200)") ||
		strings.Contains(err.Error(), "cadre_secret") {
		t.Fatalf("err = %v", err)
	}
}

func TestCreateProjectEmptyValidationErrors(t *testing.T) {
	for _, body := range []string{`{"errors":[]}`, `{}`, `nope`} {
		c := newClient(t, reply(422, body))
		_, err := c.CreateProject(context.Background(), []byte(`{}`))
		var verr *api.ValidationError
		var serr *api.StatusError
		if errors.As(err, &verr) || !errors.As(err, &serr) || serr.Status != 422 {
			t.Errorf("body %s: err = %v", body, err)
		}
	}
}

func TestResponseBodyIsBounded(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		io.WriteString(w, `{"email":"`+strings.Repeat("a", 2<<20)+`"}`)
	})
	if _, err := c.Me(context.Background()); err == nil {
		t.Fatal("expected an error for an oversized body")
	}
}

func TestMeUnauthorized(t *testing.T) {
	c := newClient(t, reply(401, `{"error":"unauthorized"}`))
	if _, err := c.Me(context.Background()); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
}

func TestUnexpectedStatusNamesStatusNotKey(t *testing.T) {
	c := newClient(t, reply(500, `boom`))
	_, err := c.Me(context.Background())
	if err == nil || !strings.Contains(err.Error(), "500") || strings.Contains(err.Error(), "cadre_secret") {
		t.Fatalf("err = %v", err)
	}
}

func TestStartLogin(t *testing.T) {
	c := newClient(t, reply(201, `{"user_code":"ABCD-EFGH","device_code":"d","verification_uri":"http://x/cli/authorize?code=ABCD-EFGH","expires_in":600,"interval":2}`))
	got, err := c.StartLogin(context.Background())
	if err != nil || got.UserCode != "ABCD-EFGH" || got.DeviceCode != "d" || got.ExpiresIn != 600 || got.Interval != 2 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestPushSpecSendsTheSpecExactly(t *testing.T) {
	spec := "  # Spec\n\n    indented \"quoted\" <b>\n"
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]string
		if err := json.Unmarshal(b, &body); err != nil || len(body) != 1 || body["spec"] != spec {
			t.Errorf("body = %s (%v)", b, err)
		}
		if r.Method != http.MethodPut || r.URL.EscapedPath() != "/api/projects/wish-reminders/milestones/2/spec" ||
			r.Header.Get("Authorization") != "Bearer cadre_secret" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("bad request: %s %s %v", r.Method, r.URL.EscapedPath(), r.Header)
		}
		w.WriteHeader(200)
		io.WriteString(w, `{"slug":"wish-reminders","milestone":2,"url":"https://cadre.example/projects/wish-reminders"}`)
	})

	got, err := c.PushSpec(context.Background(), "wish-reminders", 2, spec)

	if err != nil || got.Slug != "wish-reminders" || got.Milestone != 2 || got.URL != "https://cadre.example/projects/wish-reminders" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if string(got.Raw) != `{"slug":"wish-reminders","milestone":2,"url":"https://cadre.example/projects/wish-reminders"}` {
		t.Errorf("raw = %s", got.Raw)
	}
}

func TestPushSpecEscapesTheSlug(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/api/projects/a%2Fb%20c/milestones/1/spec" {
			t.Errorf("path = %s", r.URL.EscapedPath())
		}
		w.WriteHeader(404)
		io.WriteString(w, `{"error":"not_found"}`)
	})

	if _, err := c.PushSpec(context.Background(), "a/b c", 1, "x"); !errors.Is(err, api.ErrMilestoneNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestPushSpecStatuses(t *testing.T) {
	cases := []struct {
		status int
		body   string
		check  func(error) bool
	}{
		{404, `{"error":"not_found"}`, func(err error) bool { return errors.Is(err, api.ErrMilestoneNotFound) }},
		{401, `{"error":"unauthorized"}`, func(err error) bool { return errors.Is(err, api.ErrUnauthorized) }},
		{422, `{"errors":[{"path":"spec","message":"is required"}]}`, func(err error) bool {
			var verr *api.ValidationError
			return errors.As(err, &verr) && len(verr.Errors) == 1 && verr.Errors[0] == (api.FieldError{Path: "spec", Message: "is required"})
		}},
		{422, `{"errors":[]}`, func(err error) bool { var serr *api.StatusError; return errors.As(err, &serr) && serr.Status == 422 }},
		{500, `oops`, func(err error) bool { var serr *api.StatusError; return errors.As(err, &serr) && serr.Status == 500 }},
	}
	for _, tc := range cases {
		c := newClient(t, reply(tc.status, tc.body))
		if _, err := c.PushSpec(context.Background(), "wish-reminders", 2, "x"); !tc.check(err) {
			t.Errorf("%d %s: err = %v", tc.status, tc.body, err)
		}
	}
}
