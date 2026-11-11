package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
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
		"push spec":      func(c *api.Client) error { _, err := c.PushSpec(ctx, "wish-reminders", 2, "x", ""); return err },
		"pull spec":      func(c *api.Client) error { _, err := c.PullSpec(ctx, "wish-reminders", 2); return err },
		"link pull request": func(c *api.Client) error {
			_, err := c.LinkPullRequest(ctx, "https://github.com/colette-club/colette-api/pull/418", nil, "")
			return err
		},
		"show repository": func(c *api.Client) error { _, err := c.ShowRepository(ctx, "colette-api"); return err },
		"set target": func(c *api.Client) error {
			_, err := c.SetTarget(ctx, "colette-api", "fly", "", map[string]string{"app": "colette-api-prod"}, api.StatusURL{})
			return err
		},
		"unset target":   func(c *api.Client) error { _, err := c.UnsetTarget(ctx, "colette-site", "render", ""); return err },
		"add account":    func(c *api.Client) error { _, err := c.AddAccount(ctx, "fly", "acme", "tok"); return err },
		"remove account": func(c *api.Client) error { _, err := c.RemoveAccount(ctx, "fly", "acme"); return err },
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

	got, err := c.PushSpec(context.Background(), "wish-reminders", 2, spec, "")

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

	if _, err := c.PushSpec(context.Background(), "a/b c", 1, "x", ""); !errors.Is(err, api.ErrMilestoneNotFound) {
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
		{409, `{"error":"spec_changed"}`, func(err error) bool { return errors.Is(err, api.ErrSpecChanged) }},
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
		if _, err := c.PushSpec(context.Background(), "wish-reminders", 2, "x", ""); !tc.check(err) {
			t.Errorf("%d %s: err = %v", tc.status, tc.body, err)
		}
	}
}

func TestPushSpecSendsTheBase(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]string
		if err := json.Unmarshal(b, &body); err != nil || len(body) != 2 || body["spec"] != "x" || body["base_version"] != "v1" {
			t.Errorf("body = %s (%v)", b, err)
		}
		w.WriteHeader(200)
		io.WriteString(w, `{"slug":"wish-reminders","milestone":2,"url":"https://cadre.example/projects/wish-reminders"}`)
	})

	if _, err := c.PushSpec(context.Background(), "wish-reminders", 2, "x", "v1"); err != nil {
		t.Fatalf("err = %v", err)
	}
}

const pulledJSON = `{"slug":"wish-reminders","milestone":2,"url":"https://cadre.example/projects/wish-reminders","spec":"  lead\r\n\n    code\n\n","version":"0c6f6e2a-7a51-4c55-9a6b-2b1f3c4d5e6f","updated_by":"ada@colette.club","updated_at":"2026-10-26T15:00:00.000000Z"}`

func TestPullSpec(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/api/projects/a%2Fb%20c/milestones/2/spec" ||
			r.Header.Get("Authorization") != "Bearer cadre_secret" {
			t.Errorf("bad request: %s %s %v", r.Method, r.URL.EscapedPath(), r.Header)
		}
		w.WriteHeader(200)
		io.WriteString(w, pulledJSON)
	})

	got, err := c.PullSpec(context.Background(), "a/b c", 2)

	if err != nil || got.Slug != "wish-reminders" || got.Milestone != 2 || got.URL != "https://cadre.example/projects/wish-reminders" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got.Spec == nil || *got.Spec != "  lead\r\n\n    code\n\n" {
		t.Errorf("spec = %v", got.Spec)
	}
	if got.Version == nil || *got.Version != "0c6f6e2a-7a51-4c55-9a6b-2b1f3c4d5e6f" || got.UpdatedBy == nil || *got.UpdatedBy != "ada@colette.club" ||
		got.UpdatedAt == nil || *got.UpdatedAt != "2026-10-26T15:00:00.000000Z" {
		t.Errorf("version = %v, updated_by = %v, updated_at = %v", got.Version, got.UpdatedBy, got.UpdatedAt)
	}
	if string(got.Raw) != pulledJSON {
		t.Errorf("raw = %s", got.Raw)
	}
}

func TestPullSpecWithoutASpec(t *testing.T) {
	c := newClient(t, reply(200, `{"slug":"wish-reminders","milestone":1,"url":"https://cadre.example/projects/wish-reminders","spec":null,"version":null,"updated_by":null,"updated_at":null}`))

	got, err := c.PullSpec(context.Background(), "wish-reminders", 1)

	if err != nil || got.Spec != nil || got.Version != nil || got.UpdatedBy != nil || got.UpdatedAt != nil {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestPullSpecStatuses(t *testing.T) {
	cases := []struct {
		status int
		body   string
		check  func(error) bool
	}{
		{404, `{"error":"not_found"}`, func(err error) bool { return errors.Is(err, api.ErrMilestoneNotFound) }},
		{401, `{"error":"unauthorized"}`, func(err error) bool { return errors.Is(err, api.ErrUnauthorized) }},
		{500, `oops`, func(err error) bool { var serr *api.StatusError; return errors.As(err, &serr) && serr.Status == 500 }},
		{200, `oops`, func(err error) bool { return err != nil && strings.Contains(err.Error(), "status 200") }},
	}
	for _, tc := range cases {
		c := newClient(t, reply(tc.status, tc.body))
		if _, err := c.PullSpec(context.Background(), "wish-reminders", 2); !tc.check(err) {
			t.Errorf("%d %s: err = %v", tc.status, tc.body, err)
		}
	}
}

func TestProjectCallsStatuses(t *testing.T) {
	calls := map[string]func(c *api.Client) error{
		"pull": func(c *api.Client) error { _, err := c.PullProject(context.Background(), "wish-reminders"); return err },
		"update": func(c *api.Client) error {
			_, err := c.UpdateProject(context.Background(), "wish-reminders", []byte(`{}`))
			return err
		},
		"archive": func(c *api.Client) error {
			_, err := c.ArchiveProject(context.Background(), "wish-reminders")
			return err
		},
		"unarchive": func(c *api.Client) error {
			_, err := c.UnarchiveProject(context.Background(), "wish-reminders")
			return err
		},
	}
	for name, call := range calls {
		cases := []struct {
			status int
			body   string
			check  func(error) bool
		}{
			{200, `{"slug":"wish-reminders","name":"W","end_goal":"g","home_repo":"o/r","canvas_url":null,"version":4,"archived":false,"url":"u","milestones":[]}`, func(err error) bool { return err == nil }},
			{401, `{"error":"unauthorized"}`, func(err error) bool { return errors.Is(err, api.ErrUnauthorized) }},
			{404, `{"error":"not_found"}`, func(err error) bool { return errors.Is(err, api.ErrProjectNotFound) }},
			{500, `oops`, func(err error) bool { var serr *api.StatusError; return errors.As(err, &serr) && serr.Status == 500 }},
		}
		if name == "update" {
			cases = append(cases,
				struct {
					status int
					body   string
					check  func(error) bool
				}{409, `{"error":"project_changed","version":5}`, func(err error) bool {
					var cerr *api.ProjectChangedError
					return errors.As(err, &cerr) && cerr.Version == 5
				}},
				struct {
					status int
					body   string
					check  func(error) bool
				}{409, `{"error":"project_archived"}`, func(err error) bool { return errors.Is(err, api.ErrProjectArchived) }},
				struct {
					status int
					body   string
					check  func(error) bool
				}{422, `{"errors":[{"path":"name","message":"is required"}]}`, func(err error) bool {
					var verr *api.ValidationError
					return errors.As(err, &verr) && len(verr.Errors) == 1
				}},
				struct {
					status int
					body   string
					check  func(error) bool
				}{409, `{"error":"other"}`, func(err error) bool { var serr *api.StatusError; return errors.As(err, &serr) && serr.Status == 409 }},
			)
		}
		for _, tc := range cases {
			c := newClient(t, reply(tc.status, tc.body))
			if err := call(c); !tc.check(err) {
				t.Errorf("%s %d %s: err = %v", name, tc.status, tc.body, err)
			}
		}
	}
}

func TestProjectCallsEscapeTheSlugAndUseTheirVerbs(t *testing.T) {
	var got []string
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.EscapedPath())
		reply(200, `{}`)(w, r)
	})
	ctx := context.Background()
	c.PullProject(ctx, "a/b")
	c.UpdateProject(ctx, "a/b", []byte(`{}`))
	c.ArchiveProject(ctx, "a/b")
	c.UnarchiveProject(ctx, "a/b")
	want := []string{"GET /api/projects/a%2Fb", "PUT /api/projects/a%2Fb", "POST /api/projects/a%2Fb/archive", "POST /api/projects/a%2Fb/unarchive"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("requests = %q", got)
	}
}

const linkedJSON = `{"repo":"colette-api","number":418,"url":"https://github.com/colette-club/colette-api/pull/418","milestone":"wish-reminders/2","project_url":"https://cadre.example/projects/wish-reminders"}`

func TestLinkPullRequest(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(b, &body); err != nil || len(body) != 2 ||
			body["url"] != "https://github.com/colette-club/colette-api/pull/418" || body["milestone"] != "wish-reminders/2" {
			t.Errorf("body = %s (%v)", b, err)
		}
		if r.Method != http.MethodPost || r.URL.EscapedPath() != "/api/pull-requests/link" || r.Header.Get("Authorization") != "Bearer cadre_secret" {
			t.Errorf("bad request: %s %s %v", r.Method, r.URL.EscapedPath(), r.Header)
		}
		w.WriteHeader(200)
		io.WriteString(w, linkedJSON)
	})
	milestone := "wish-reminders/2"

	got, err := c.LinkPullRequest(context.Background(), "https://github.com/colette-club/colette-api/pull/418", &milestone, "")

	if err != nil || got.Repo != "colette-api" || got.Number != 418 || got.URL != "https://github.com/colette-club/colette-api/pull/418" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got.Milestone == nil || *got.Milestone != "wish-reminders/2" || got.ProjectURL == nil || *got.ProjectURL != "https://cadre.example/projects/wish-reminders" {
		t.Errorf("milestone = %v, project_url = %v", got.Milestone, got.ProjectURL)
	}
	if string(got.Raw) != linkedJSON {
		t.Errorf("raw = %s", got.Raw)
	}
}

func TestLinkPullRequestSendsNullToUnlink(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(b, &body); err != nil {
			t.Errorf("body = %s (%v)", b, err)
		}
		milestone, sent := body["milestone"]
		if len(body) != 2 || !sent || milestone != nil {
			t.Errorf("body = %s", b)
		}
		w.WriteHeader(200)
		io.WriteString(w, `{"repo":"colette-api","number":418,"url":"https://github.com/colette-club/colette-api/pull/418","milestone":null,"project_url":null}`)
	})

	got, err := c.LinkPullRequest(context.Background(), "https://github.com/colette-club/colette-api/pull/418", nil, "")

	if err != nil || got.Milestone != nil || got.ProjectURL != nil {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestLinkPullRequestStatuses(t *testing.T) {
	cases := []struct {
		status int
		body   string
		check  func(error) bool
	}{
		{404, `{"error":"milestone_not_found"}`, func(err error) bool { return errors.Is(err, api.ErrMilestoneNotFound) }},
		{404, `{"error":"pull_request_not_found"}`, func(err error) bool { return errors.Is(err, api.ErrPullRequestNotFound) }},
		{404, `{"error":"not_found"}`, func(err error) bool { var serr *api.StatusError; return errors.As(err, &serr) && serr.Status == 404 }},
		{503, `{"error":"github_not_connected"}`, func(err error) bool { return errors.Is(err, api.ErrGitHubNotConnected) }},
		{502, `{"error":"github_unreachable"}`, func(err error) bool { return errors.Is(err, api.ErrGitHubUnreachable) }},
		{401, `{"error":"unauthorized"}`, func(err error) bool { return errors.Is(err, api.ErrUnauthorized) }},
		{422, `{"errors":[{"path":"url","message":"is in a repo no project names"}]}`, func(err error) bool {
			var verr *api.ValidationError
			return errors.As(err, &verr) && len(verr.Errors) == 1 && verr.Errors[0] == (api.FieldError{Path: "url", Message: "is in a repo no project names"})
		}},
		{500, `oops`, func(err error) bool { var serr *api.StatusError; return errors.As(err, &serr) && serr.Status == 500 }},
		{200, `oops`, func(err error) bool { return err != nil && strings.Contains(err.Error(), "status 200") }},
	}
	for _, tc := range cases {
		c := newClient(t, reply(tc.status, tc.body))
		if _, err := c.LinkPullRequest(context.Background(), "https://github.com/colette-club/colette-api/pull/418", nil, ""); !tc.check(err) {
			t.Errorf("%d %s: err = %v", tc.status, tc.body, err)
		}
	}
}

const repositoryJSON = `{"name":"colette-api","github_full_name":"colette-club/colette-api","targets":[{"platform":"fly","settings":{"app":"colette-api-prod"},"tracked":true},{"platform":"render","settings":{},"tracked":false}]}`

func TestShowRepository(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/api/repositories/colette-api" || r.Header.Get("Authorization") != "Bearer cadre_secret" {
			t.Errorf("bad request: %s %s %v", r.Method, r.URL.EscapedPath(), r.Header)
		}
		w.WriteHeader(200)
		io.WriteString(w, repositoryJSON)
	})

	got, err := c.ShowRepository(context.Background(), "colette-api")

	if err != nil || got.Name != "colette-api" || got.GitHubFullName != "colette-club/colette-api" || len(got.Targets) != 2 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if fly := got.Targets[0]; fly.Platform != "fly" || fly.Settings["app"] != "colette-api-prod" || !fly.Tracked {
		t.Errorf("fly = %+v", fly)
	}
	if render := got.Targets[1]; render.Platform != "render" || len(render.Settings) != 0 || render.Tracked {
		t.Errorf("render = %+v", render)
	}
	if string(got.Raw) != repositoryJSON {
		t.Errorf("raw = %s", got.Raw)
	}
}

func TestShowRepositoryEscapesTheName(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/api/repositories/a%2Fb" {
			t.Errorf("path = %s", r.URL.EscapedPath())
		}
		w.WriteHeader(200)
		io.WriteString(w, repositoryJSON)
	})
	if _, err := c.ShowRepository(context.Background(), "a/b"); err != nil {
		t.Fatal(err)
	}
}

const targetJSON = `{"repo":"colette-api","platform":"fly","settings":{"app":"colette-api-prod"},"tracked":true}`

func TestSetTarget(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if string(b) != `{"settings":{"app":"colette-api-prod"}}` {
			t.Errorf("body = %s", b)
		}
		if r.Method != http.MethodPut || r.URL.EscapedPath() != "/api/repositories/colette-api/targets/fly" || r.Header.Get("Authorization") != "Bearer cadre_secret" {
			t.Errorf("bad request: %s %s %v", r.Method, r.URL.EscapedPath(), r.Header)
		}
		w.WriteHeader(200)
		io.WriteString(w, targetJSON)
	})

	got, err := c.SetTarget(context.Background(), "colette-api", "fly", "", map[string]string{"app": "colette-api-prod"}, api.StatusURL{})

	if err != nil || got.Repo != "colette-api" || got.Platform != "fly" || got.Settings["app"] != "colette-api-prod" || !got.Tracked {
		t.Fatalf("got %+v, %v", got, err)
	}
	if string(got.Raw) != targetJSON {
		t.Errorf("raw = %s", got.Raw)
	}
}

func TestSetTargetSendsEmptySettingsAsAnObject(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if string(b) != `{"settings":{}}` {
			t.Errorf("body = %s", b)
		}
		w.WriteHeader(200)
		io.WriteString(w, `{"repo":"colette-site","platform":"render","settings":{},"tracked":false}`)
	})
	if _, err := c.SetTarget(context.Background(), "colette-site", "render", "", nil, api.StatusURL{}); err != nil {
		t.Fatal(err)
	}
}

func TestSetTargetStatuses(t *testing.T) {
	cases := []struct {
		status int
		body   string
		check  func(error) bool
	}{
		{404, `{"error":"repository_not_found"}`, func(err error) bool { return errors.Is(err, api.ErrRepositoryNotFound) }},
		{404, `{"error":"other"}`, func(err error) bool { var serr *api.StatusError; return errors.As(err, &serr) && serr.Status == 404 }},
		{503, `{"error":"platform_not_connected","platform":"fly"}`, func(err error) bool { return errors.Is(err, api.ErrPlatformNotConnected) }},
		{502, `{"error":"platform_unreachable","platform":"fly"}`, func(err error) bool { return errors.Is(err, api.ErrPlatformUnreachable) }},
		{401, `{"error":"unauthorized"}`, func(err error) bool { return errors.Is(err, api.ErrUnauthorized) }},
		{422, `{"errors":[{"path":"settings.app","message":"is not an app in the org"}]}`, func(err error) bool {
			var verr *api.ValidationError
			return errors.As(err, &verr) && len(verr.Errors) == 1 && verr.Errors[0] == (api.FieldError{Path: "settings.app", Message: "is not an app in the org"})
		}},
		{500, `oops`, func(err error) bool { var serr *api.StatusError; return errors.As(err, &serr) && serr.Status == 500 }},
		{200, `oops`, func(err error) bool { return err != nil && strings.Contains(err.Error(), "status 200") }},
	}
	for _, tc := range cases {
		c := newClient(t, reply(tc.status, tc.body))
		if _, err := c.SetTarget(context.Background(), "colette-api", "fly", "", nil, api.StatusURL{}); !tc.check(err) {
			t.Errorf("%d %s: err = %v", tc.status, tc.body, err)
		}
	}
}

func TestUnsetTarget(t *testing.T) {
	const body = `{"repo":"colette-site","platform":"render"}`
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.EscapedPath() != "/api/repositories/colette-site/targets/render" || r.Header.Get("Authorization") != "Bearer cadre_secret" {
			t.Errorf("bad request: %s %s %v", r.Method, r.URL.EscapedPath(), r.Header)
		}
		w.WriteHeader(200)
		io.WriteString(w, body)
	})

	got, err := c.UnsetTarget(context.Background(), "colette-site", "render", "")

	if err != nil || got.Repo != "colette-site" || got.Platform != "render" || string(got.Raw) != body {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestUnsetTargetStatuses(t *testing.T) {
	cases := []struct {
		status int
		body   string
		check  func(error) bool
	}{
		{404, `{"error":"repository_not_found"}`, func(err error) bool { return errors.Is(err, api.ErrRepositoryNotFound) }},
		{404, `{"error":"target_not_found"}`, func(err error) bool { return errors.Is(err, api.ErrTargetNotFound) }},
		{404, `{"error":"other"}`, func(err error) bool { var serr *api.StatusError; return errors.As(err, &serr) && serr.Status == 404 }},
		{401, `{"error":"unauthorized"}`, func(err error) bool { return errors.Is(err, api.ErrUnauthorized) }},
		{500, `oops`, func(err error) bool { var serr *api.StatusError; return errors.As(err, &serr) && serr.Status == 500 }},
	}
	for _, tc := range cases {
		c := newClient(t, reply(tc.status, tc.body))
		if _, err := c.UnsetTarget(context.Background(), "colette-site", "render", ""); !tc.check(err) {
			t.Errorf("%d %s: err = %v", tc.status, tc.body, err)
		}
	}
}

func TestShowRepositoryStatuses(t *testing.T) {
	cases := []struct {
		status int
		body   string
		check  func(error) bool
	}{
		{404, `{"error":"repository_not_found"}`, func(err error) bool { return errors.Is(err, api.ErrRepositoryNotFound) }},
		{401, `{"error":"unauthorized"}`, func(err error) bool { return errors.Is(err, api.ErrUnauthorized) }},
		{500, `oops`, func(err error) bool { var serr *api.StatusError; return errors.As(err, &serr) && serr.Status == 500 }},
	}
	for _, tc := range cases {
		c := newClient(t, reply(tc.status, tc.body))
		if _, err := c.ShowRepository(context.Background(), "colette-api"); !tc.check(err) {
			t.Errorf("%d %s: err = %v", tc.status, tc.body, err)
		}
	}
}

func TestAddAccount(t *testing.T) {
	const body = `{"platform":"fly","name":"acme","apps":4}`
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPut || r.URL.EscapedPath() != "/api/platforms/fly/accounts/acme%2Fx" || r.Header.Get("Authorization") != "Bearer cadre_secret" || string(b) != `{"token":"tok"}` {
			t.Errorf("bad request: %s %s %s %v", r.Method, r.URL.EscapedPath(), b, r.Header)
		}
		w.WriteHeader(200)
		io.WriteString(w, body)
	})
	got, err := c.AddAccount(context.Background(), "fly", "acme/x", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if got.Platform != "fly" || got.Name != "acme" || got.Apps != 4 || string(got.Raw) != body {
		t.Errorf("got %+v", got)
	}
}

func TestAddAccountStatuses(t *testing.T) {
	cases := []struct {
		status int
		body   string
		check  func(error) bool
	}{
		{404, `{"error":"platform_not_found"}`, func(err error) bool { return errors.Is(err, api.ErrPlatformNotFound) }},
		{404, `{"error":"other"}`, func(err error) bool { var serr *api.StatusError; return errors.As(err, &serr) && serr.Status == 404 }},
		{422, `{"error":"token_refused"}`, func(err error) bool { return errors.Is(err, api.ErrTokenRefused) }},
		{422, `{"error":"org_not_found"}`, func(err error) bool { return errors.Is(err, api.ErrOrgNotFound) }},
		{422, `{"errors":[{"path":"name","message":"is invalid"}]}`, func(err error) bool {
			var verr *api.ValidationError
			return errors.As(err, &verr) && len(verr.Errors) == 1 && verr.Errors[0] == (api.FieldError{Path: "name", Message: "is invalid"})
		}},
		{422, `{"error":"other"}`, func(err error) bool { var serr *api.StatusError; return errors.As(err, &serr) && serr.Status == 422 }},
		{502, `{"error":"platform_unreachable","platform":"fly"}`, func(err error) bool { return errors.Is(err, api.ErrPlatformUnreachable) }},
		{401, `{"error":"unauthorized"}`, func(err error) bool { return errors.Is(err, api.ErrUnauthorized) }},
		{500, `oops`, func(err error) bool { var serr *api.StatusError; return errors.As(err, &serr) && serr.Status == 500 }},
		{200, `oops`, func(err error) bool { return err != nil && strings.Contains(err.Error(), "status 200") }},
	}
	for _, tc := range cases {
		c := newClient(t, reply(tc.status, tc.body))
		if _, err := c.AddAccount(context.Background(), "fly", "acme", "tok"); !tc.check(err) {
			t.Errorf("%d %s: err = %v", tc.status, tc.body, err)
		}
	}
}

func TestRemoveAccount(t *testing.T) {
	const body = `{"platform":"fly","name":"acme"}`
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.EscapedPath() != "/api/platforms/fly/accounts/acme" || r.Header.Get("Authorization") != "Bearer cadre_secret" {
			t.Errorf("bad request: %s %s %v", r.Method, r.URL.EscapedPath(), r.Header)
		}
		w.WriteHeader(200)
		io.WriteString(w, body)
	})
	got, err := c.RemoveAccount(context.Background(), "fly", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if got.Platform != "fly" || got.Name != "acme" || string(got.Raw) != body {
		t.Errorf("got %+v", got)
	}
}

func TestRemoveAccountStatuses(t *testing.T) {
	cases := []struct {
		status int
		body   string
		check  func(error) bool
	}{
		{404, `{"error":"account_not_found"}`, func(err error) bool { return errors.Is(err, api.ErrAccountNotFound) }},
		{404, `{"error":"platform_not_found"}`, func(err error) bool { return errors.Is(err, api.ErrPlatformNotFound) }},
		{409, `{"error":"account_in_use","repos":["colette-api","colette-web"]}`, func(err error) bool {
			var use *api.AccountInUseError
			return errors.As(err, &use) && slices.Equal(use.Repos, []string{"colette-api", "colette-web"})
		}},
		{409, `{"error":"other"}`, func(err error) bool { var serr *api.StatusError; return errors.As(err, &serr) && serr.Status == 409 }},
		{401, `{"error":"unauthorized"}`, func(err error) bool { return errors.Is(err, api.ErrUnauthorized) }},
		{500, `oops`, func(err error) bool { var serr *api.StatusError; return errors.As(err, &serr) && serr.Status == 500 }},
	}
	for _, tc := range cases {
		c := newClient(t, reply(tc.status, tc.body))
		if _, err := c.RemoveAccount(context.Background(), "fly", "acme"); !tc.check(err) {
			t.Errorf("%d %s: err = %v", tc.status, tc.body, err)
		}
	}
}

func TestTargetCarriesItsAccount(t *testing.T) {
	c := newClient(t, reply(200, `{"name":"colette-api","github_full_name":"x/y","targets":[{"platform":"fly","settings":{"app":"a"},"tracked":true,"account":"colette"},{"platform":"render","settings":{},"tracked":false,"account":null}]}`))
	got, err := c.ShowRepository(context.Background(), "colette-api")
	if err != nil {
		t.Fatal(err)
	}
	if a := got.Targets[0].Account; a == nil || *a != "colette" || got.Targets[1].Account != nil {
		t.Errorf("targets = %+v", got.Targets)
	}
}

func TestNextDeploy(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/api/repositories/colette-api/next-deploy" || r.Header.Get("Authorization") != "Bearer cadre_secret" {
			t.Errorf("bad request: %s %s %v", r.Method, r.URL.EscapedPath(), r.Header)
		}
		w.WriteHeader(200)
		io.WriteString(w, `{"repo":"colette-api","apps":["colette-api"],"before_deploying":[{"number":418,"url":"https://github.com/colette-club/colette-api/pull/418","text":"Set SENDGRID_KEY on colette-api"}],"blocked":[]}`)
	})

	got, err := c.NextDeploy(context.Background(), "colette-api", "")

	want := api.BeforeDeployingItem{Number: 418, URL: "https://github.com/colette-club/colette-api/pull/418", Text: "Set SENDGRID_KEY on colette-api"}
	if err != nil || got.Repo != "colette-api" || len(got.BeforeDeploying) != 1 || got.BeforeDeploying[0] != want {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestNextDeployWithNothingOpen(t *testing.T) {
	c := newClient(t, reply(200, `{"repo":"colette-api","apps":[],"before_deploying":[],"blocked":[]}`))

	got, err := c.NextDeploy(context.Background(), "colette-api", "")

	if err != nil || got.BeforeDeploying == nil || len(got.BeforeDeploying) != 0 || got.Blocked == nil || len(got.Blocked) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// A 200 with no list says nothing about what is open, so it must not read as nothing being open.
func TestNextDeployWithoutAList(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"repo":"colette-api"}`,
		`{"repo":"colette-api","before_deploying":null}`,
		`{"repo":"colette-api","open_items":[]}`,
	} {
		c := newClient(t, reply(200, body))
		if _, err := c.NextDeploy(context.Background(), "colette-api", ""); err == nil || !strings.Contains(err.Error(), "unexpected response") {
			t.Errorf("%s: err = %v", body, err)
		}
	}
}

// One app's answer: its apps, its items and the pull requests the queue shows Blocked, each with the step it waits for,
// whose pull request may be unknown.
func TestNextDeployAsksForOneApp(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/api/repositories/kinfield/next-deploy" || r.URL.RawQuery != "app=kinfield-family" {
			t.Errorf("bad request: %s %s?%s", r.Method, r.URL.EscapedPath(), r.URL.RawQuery)
		}
		w.WriteHeader(200)
		io.WriteString(w, `{"repo":"kinfield","apps":["kinfield-family"],"before_deploying":[],"blocked":[
			{"number":214,"url":"https://github.com/colette-club/kinfield/pull/214","waits_for":{"name":"kinfield","number":214}},
			{"number":219,"url":"https://github.com/colette-club/kinfield/pull/219","waits_for":{"name":"club-mobile","number":null}}
		]}`)
	})

	got, err := c.NextDeploy(context.Background(), "kinfield", "kinfield-family")

	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Apps, []string{"kinfield-family"}) || len(got.Blocked) != 2 {
		t.Fatalf("got %+v", got)
	}
	first, second := got.Blocked[0], got.Blocked[1]
	if first.Number != 214 || first.URL != "https://github.com/colette-club/kinfield/pull/214" || first.WaitsFor.Name != "kinfield" ||
		first.WaitsFor.Number == nil || *first.WaitsFor.Number != 214 {
		t.Errorf("first = %+v", first)
	}
	if second.Number != 219 || second.WaitsFor.Name != "club-mobile" || second.WaitsFor.Number != nil {
		t.Errorf("second = %+v", second)
	}
}

func TestNextDeployAsksForTheRepoWithNoApp(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/api/repositories/kinfield/next-deploy" || r.URL.RawQuery != "" {
			t.Errorf("bad request: %s?%s", r.URL.EscapedPath(), r.URL.RawQuery)
		}
		w.WriteHeader(200)
		io.WriteString(w, `{"repo":"kinfield","apps":["kinfield","kinfield-family"],"before_deploying":[],"blocked":[]}`)
	})

	got, err := c.NextDeploy(context.Background(), "kinfield", "")

	if err != nil || !slices.Equal(got.Apps, []string{"kinfield", "kinfield-family"}) {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// The app's name goes in the query escaped, so no character in it can add a parameter.
func TestNextDeployEscapesTheApp(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query(); len(got) != 1 || got.Get("app") != "web&all=1 #x" {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		w.WriteHeader(200)
		io.WriteString(w, `{"repo":"match-day","apps":[],"before_deploying":[],"blocked":[]}`)
	})

	if _, err := c.NextDeploy(context.Background(), "match-day", "web&all=1 #x"); err != nil {
		t.Fatal(err)
	}
}

// A 200 with no Blocked list says nothing about what is Blocked, so it must not read as nothing being Blocked.
func TestNextDeployWithoutBlockedIsAnError(t *testing.T) {
	for _, body := range []string{
		`{"repo":"colette-api","apps":[],"before_deploying":[]}`,
		`{"repo":"colette-api","apps":[],"before_deploying":[],"blocked":null}`,
	} {
		c := newClient(t, reply(200, body))
		if _, err := c.NextDeploy(context.Background(), "colette-api", ""); err == nil || !strings.Contains(err.Error(), "unexpected response") {
			t.Errorf("%s: err = %v", body, err)
		}
	}
}

func TestNextDeployErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   error
	}{
		{404, `{"error":"repository_not_found"}`, api.ErrRepositoryNotFound},
		{404, `{"error":"target_not_found"}`, api.ErrTargetNotFound},
		{401, `{"error":"unauthorized"}`, api.ErrUnauthorized},
	}
	for _, tc := range cases {
		c := newClient(t, reply(tc.status, tc.body))
		if _, err := c.NextDeploy(context.Background(), "colette-api", "kinfield-family"); !errors.Is(err, tc.want) {
			t.Errorf("%d: err = %v", tc.status, err)
		}
	}
}

func TestSetTargetSendsAStatusURLAndReadsItBack(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if string(b) != `{"settings":{"app":"colette-api-prod"},"status_url":"https://app.test/status"}` {
			t.Errorf("body = %s", b)
		}
		w.WriteHeader(200)
		io.WriteString(w, `{"repo":"colette-api","platform":"fly","settings":{"app":"colette-api-prod"},"tracked":true,"status_url":"https://app.test/status","live":"not_live","not_live_since":"2026-11-05T08:14:00Z"}`)
	})

	got, err := c.SetTarget(context.Background(), "colette-api", "fly", "", map[string]string{"app": "colette-api-prod"}, api.StatusURL{URL: "https://app.test/status"})

	if err != nil || got.StatusURL != "https://app.test/status" || got.Live != "not_live" || got.NotLiveSince == nil || got.NotLiveSince.UTC().Format("15:04") != "08:14" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestSetTargetSendsAVercelNameAndReadsItBack(t *testing.T) {
	const body = `{"repo":"colette-agents","platform":"vercel","name":"linear","settings":{"dir":"apps/linear","project":"prj_1"},"account":"colette-club","tracked":true}`
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if string(b) != `{"name":"linear","settings":{"dir":"apps/linear","project":"prj_1"}}` {
			t.Errorf("body = %s", b)
		}
		if r.Method != http.MethodPut || r.URL.EscapedPath() != "/api/repositories/colette-agents/targets/vercel" {
			t.Errorf("bad request: %s %s", r.Method, r.URL.EscapedPath())
		}
		w.WriteHeader(200)
		io.WriteString(w, body)
	})

	got, err := c.SetTarget(context.Background(), "colette-agents", "vercel", "linear", map[string]string{"project": "prj_1", "dir": "apps/linear"}, api.StatusURL{})

	if err != nil || got.Name != "linear" || got.Platform != "vercel" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestSetTargetLeavesTheNameOutWhenEmpty(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if string(b) != `{"settings":{"app":"colette-api-prod"}}` {
			t.Errorf("body = %s", b)
		}
		w.WriteHeader(200)
		io.WriteString(w, targetJSON)
	})
	if _, err := c.SetTarget(context.Background(), "colette-api", "fly", "", map[string]string{"app": "colette-api-prod"}, api.StatusURL{}); err != nil {
		t.Fatal(err)
	}
}

func TestUnsetTargetSendsTheNameInTheQuery(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.EscapedPath() != "/api/repositories/colette-agents/targets/vercel" || r.URL.RawQuery != "name=linear+app" {
			t.Errorf("bad request: %s %s?%s", r.Method, r.URL.EscapedPath(), r.URL.RawQuery)
		}
		w.WriteHeader(200)
		io.WriteString(w, `{"repo":"colette-agents","platform":"vercel"}`)
	})
	if _, err := c.UnsetTarget(context.Background(), "colette-agents", "vercel", "linear app"); err != nil {
		t.Fatal(err)
	}
}

func TestUnsetTargetWithoutANameSendsNoQuery(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		w.WriteHeader(200)
		io.WriteString(w, `{"repo":"colette-site","platform":"render"}`)
	})
	if _, err := c.UnsetTarget(context.Background(), "colette-site", "render", ""); err != nil {
		t.Fatal(err)
	}
}

func TestUnsetAppDeletesItByName(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.EscapedPath() != "/api/repositories/kinfield/apps/linear%20app" {
			t.Errorf("bad request: %s %s", r.Method, r.URL.EscapedPath())
		}
		w.WriteHeader(200)
		io.WriteString(w, `{"repo":"kinfield","platform":"fly"}`)
	})
	got, err := c.UnsetApp(context.Background(), "kinfield", "linear app")
	if err != nil || got.Repo != "kinfield" || got.Platform != "fly" || string(got.Raw) != `{"repo":"kinfield","platform":"fly"}` {
		t.Errorf("got %+v, err %v", got, err)
	}
}

func TestUnsetAppStatuses(t *testing.T) {
	cases := []struct {
		status int
		body   string
		check  func(error) bool
	}{
		{404, `{"error":"repository_not_found"}`, func(err error) bool { return errors.Is(err, api.ErrRepositoryNotFound) }},
		{404, `{"error":"target_not_found"}`, func(err error) bool { return errors.Is(err, api.ErrTargetNotFound) }},
		{401, `{"error":"unauthorized"}`, func(err error) bool { return errors.Is(err, api.ErrUnauthorized) }},
	}
	for _, tc := range cases {
		c := newClient(t, reply(tc.status, tc.body))
		if _, err := c.UnsetApp(context.Background(), "kinfield", "linear"); !tc.check(err) {
			t.Errorf("%d %s: err = %v", tc.status, tc.body, err)
		}
	}
}

func TestShowRepositoryReadsAVercelTargetsName(t *testing.T) {
	c := newClient(t, reply(200, `{"name":"colette-agents","github_full_name":"colette-club/colette-agents","targets":[{"platform":"vercel","name":"linear","settings":{},"tracked":true},{"platform":"fly","name":null,"settings":{},"tracked":true}]}`))
	got, err := c.ShowRepository(context.Background(), "colette-agents")
	if err != nil || len(got.Targets) != 2 || got.Targets[0].Name != "linear" || got.Targets[1].Name != "" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestLinkPullRequestSendsApp(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if want := `{"url":"https://github.com/acme/shop-api/pull/37","milestone":"live-scores/2","app":"api"}`; string(b) != want {
			t.Errorf("body = %s, want %s", b, want)
		}
		w.WriteHeader(200)
		io.WriteString(w, `{"repo":"match-day","number":37,"url":"https://github.com/acme/shop-api/pull/37","milestone":"live-scores/2","app":"api","project_url":"https://cadre.example/projects/live-scores"}`)
	})
	milestone := "live-scores/2"

	got, err := c.LinkPullRequest(context.Background(), "https://github.com/acme/shop-api/pull/37", &milestone, "api")

	if err != nil || got.App == nil || *got.App != "api" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestSetTargetSendsNullForNoStatusURL(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if string(b) != `{"settings":{"app":"colette-api-prod"},"status_url":null}` {
			t.Errorf("body = %s", b)
		}
		w.WriteHeader(200)
		io.WriteString(w, targetJSON)
	})
	if _, err := c.SetTarget(context.Background(), "colette-api", "fly", "", map[string]string{"app": "colette-api-prod"}, api.StatusURL{None: true}); err != nil {
		t.Fatal(err)
	}
}

func TestSetTargetLeavesTheStatusURLOutForTheDefault(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if string(b) != `{"settings":{"app":"colette-api-prod"}}` {
			t.Errorf("body = %s", b)
		}
		w.WriteHeader(200)
		io.WriteString(w, targetJSON)
	})
	if _, err := c.SetTarget(context.Background(), "colette-api", "fly", "", map[string]string{"app": "colette-api-prod"}, api.StatusURL{}); err != nil {
		t.Fatal(err)
	}
}

func TestSetTargetReadsTheStatusURLNote(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		io.WriteString(w, `{"repo":"colette-site","platform":"vercel","settings":{"project":"prj_1"},"tracked":true,"status_url":null,"status_url_note":"mosh.colette.club answers 302, so pass --status-url"}`)
	})

	got, err := c.SetTarget(context.Background(), "colette-site", "vercel", "site", map[string]string{"project": "prj_1"}, api.StatusURL{})

	if err != nil || got.StatusURLNote != "mosh.colette.club answers 302, so pass --status-url" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// A set of a Vercel app with no status URL has the server read the project's domains and call one, which can take
// longer than the other calls' timeout; the client must not give up on it first.
func TestSetTargetWaitsLongerThanOtherCalls(t *testing.T) {
	api.SetTimeouts(t, 50*time.Millisecond, 3*time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"repo":"colette-api","platform":"vercel","settings":{},"tracked":true}`))
	}))
	t.Cleanup(srv.Close)
	c := &api.Client{BaseURL: srv.URL, APIKey: "cadre_secret", UserAgent: "cadre-cli/test"}

	if _, err := c.SetTarget(context.Background(), "colette-api", "vercel", "", nil, api.StatusURL{}); err != nil {
		t.Errorf("SetTarget: %v", err)
	}
	if _, err := c.UnsetTarget(context.Background(), "colette-api", "vercel", ""); err == nil {
		t.Error("UnsetTarget was not cut off at the default timeout")
	}
}

// A Google Play account is named by an email, which must stay one path segment.
func TestAccountNameIsOnePathSegment(t *testing.T) {
	var paths []string
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath())
		w.WriteHeader(200)
		io.WriteString(w, `{"platform":"google_play","name":"x","apps":0}`)
	})
	for _, name := range []string{"cadre-reader+a@colette.iam.gserviceaccount.com", "a/b@c.d"} {
		if _, err := c.AddAccount(context.Background(), "google_play", name, "tok"); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"/api/platforms/google_play/accounts/cadre-reader+a@colette.iam.gserviceaccount.com", "/api/platforms/google_play/accounts/a%2Fb@c.d"}
	if !slices.Equal(paths, want) {
		t.Errorf("paths = %q, want %q", paths, want)
	}
}

func TestSetTargetDecodesStores(t *testing.T) {
	body := `{"repo":"club-mobile","platform":"mobile","name":"club-mobile","settings":{"app_store_app":"6443635924"},"tracked":false,"account":null,"stores":[` +
		`{"store":"app_store","label":"App Store","app":"6443635924","account":"2X9R4HXF34","public_build":{"version":"1.8.15","build_number":6188,"since":"2026-11-02T09:40:00Z"}},` +
		`{"store":"google_play","label":"Google Play","app":"club.colette.club","account":"cadre-reader@colette.iam.gserviceaccount.com","public_build":{"version":null,"build_number":6185,"since":"2026-11-02T09:40:00Z"}},` +
		`{"store":"other","label":"Other","app":"x","account":"y","public_build":null}]}`
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		io.WriteString(w, body)
	})

	got, err := c.SetTarget(context.Background(), "club-mobile", "mobile", "", map[string]string{"app_store_app": "6443635924"}, api.StatusURL{})

	if err != nil || len(got.Stores) != 3 {
		t.Fatalf("got %+v, %v", got, err)
	}
	first, second, third := got.Stores[0], got.Stores[1], got.Stores[2]
	if first.Store != "app_store" || first.Label != "App Store" || first.App != "6443635924" || first.Account != "2X9R4HXF34" {
		t.Errorf("first = %+v", first)
	}
	if b := first.PublicBuild; b == nil || b.Version == nil || *b.Version != "1.8.15" || b.BuildNumber != 6188 || b.Since.Format(time.RFC3339) != "2026-11-02T09:40:00Z" {
		t.Errorf("first build = %+v", first.PublicBuild)
	}
	if b := second.PublicBuild; b == nil || b.Version != nil || b.BuildNumber != 6185 {
		t.Errorf("second build = %+v", second.PublicBuild)
	}
	if third.PublicBuild != nil {
		t.Errorf("third build = %+v", third.PublicBuild)
	}
}

func TestSetTargetDecodesABuildsTagAndCommit(t *testing.T) {
	body := `{"repo":"club-mobile","platform":"mobile","name":"club-mobile","settings":{},"tracked":false,"account":null,"stores":[` +
		`{"store":"app_store","label":"App Store","app":"1","account":"k","public_build":{"version":"1.8.15","build_number":6188,"since":"2026-11-02T09:40:00Z","tag":"prod-1.8.15","commit":"da659a5c0ffee0123456789abcdef0123456789a"}},` +
		`{"store":"google_play","label":"Google Play","app":"2","account":"k","public_build":{"version":"1.8.15","build_number":6188,"since":"2026-11-02T09:40:00Z","tag":null,"commit":null}},` +
		`{"store":"other","label":"Other","app":"3","account":"k","public_build":{"version":"1.8.14","build_number":6100,"since":"2026-11-01T09:40:00Z"}}]}`
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		io.WriteString(w, body)
	})

	got, err := c.SetTarget(context.Background(), "club-mobile", "mobile", "", map[string]string{}, api.StatusURL{})

	if err != nil || len(got.Stores) != 3 {
		t.Fatalf("got %+v, %v", got, err)
	}
	tagged, untagged, older := got.Stores[0].PublicBuild, got.Stores[1].PublicBuild, got.Stores[2].PublicBuild
	if tagged.Tag == nil || *tagged.Tag != "prod-1.8.15" || tagged.Commit == nil || *tagged.Commit != "da659a5c0ffee0123456789abcdef0123456789a" {
		t.Errorf("tagged = %+v", tagged)
	}
	if untagged.Tag != nil || untagged.Commit != nil {
		t.Errorf("untagged = %+v", untagged)
	}
	if older.Tag != nil || older.Commit != nil {
		t.Errorf("older = %+v", older)
	}
}
