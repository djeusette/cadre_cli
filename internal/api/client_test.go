package api_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/colette-club/cadre/cli/internal/api"
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
