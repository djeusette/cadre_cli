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
		"push spec":      func(c *api.Client) error { _, err := c.PushSpec(ctx, "wish-reminders", 2, "x", ""); return err },
		"pull spec":      func(c *api.Client) error { _, err := c.PullSpec(ctx, "wish-reminders", 2); return err },
		"link pull request": func(c *api.Client) error {
			_, err := c.LinkPullRequest(ctx, "https://github.com/colette-club/colette-api/pull/418", nil)
			return err
		},
		"show repository": func(c *api.Client) error { _, err := c.ShowRepository(ctx, "colette-api"); return err },
		"set target": func(c *api.Client) error {
			_, err := c.SetTarget(ctx, "colette-api", "fly", map[string]string{"app": "colette-api-prod"})
			return err
		},
		"unset target": func(c *api.Client) error { _, err := c.UnsetTarget(ctx, "colette-site", "render"); return err },
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

	got, err := c.LinkPullRequest(context.Background(), "https://github.com/colette-club/colette-api/pull/418", &milestone)

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

	got, err := c.LinkPullRequest(context.Background(), "https://github.com/colette-club/colette-api/pull/418", nil)

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
		if _, err := c.LinkPullRequest(context.Background(), "https://github.com/colette-club/colette-api/pull/418", nil); !tc.check(err) {
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

	got, err := c.SetTarget(context.Background(), "colette-api", "fly", map[string]string{"app": "colette-api-prod"})

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
	if _, err := c.SetTarget(context.Background(), "colette-site", "render", nil); err != nil {
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
		{422, `{"errors":[{"path":"app","message":"is not an app in the org"}]}`, func(err error) bool {
			var verr *api.ValidationError
			return errors.As(err, &verr) && len(verr.Errors) == 1 && verr.Errors[0] == (api.FieldError{Path: "app", Message: "is not an app in the org"})
		}},
		{500, `oops`, func(err error) bool { var serr *api.StatusError; return errors.As(err, &serr) && serr.Status == 500 }},
		{200, `oops`, func(err error) bool { return err != nil && strings.Contains(err.Error(), "status 200") }},
	}
	for _, tc := range cases {
		c := newClient(t, reply(tc.status, tc.body))
		if _, err := c.SetTarget(context.Background(), "colette-api", "fly", nil); !tc.check(err) {
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

	got, err := c.UnsetTarget(context.Background(), "colette-site", "render")

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
		if _, err := c.UnsetTarget(context.Background(), "colette-site", "render"); !tc.check(err) {
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
