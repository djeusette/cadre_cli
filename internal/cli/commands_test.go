package cli_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colette-club/cadre/cli/internal/cli"
	"github.com/colette-club/cadre/cli/internal/config"
)

type harness struct {
	deps    cli.Env
	out     *strings.Builder
	errb    *strings.Builder
	cfgPath string
	opened  []string
}

func newHarness(t *testing.T, srv *httptest.Server, cfg *config.Config) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{out: &strings.Builder{}, errb: &strings.Builder{}, cfgPath: filepath.Join(dir, "cadre", "config.json")}
	if cfg != nil {
		if err := config.Save(h.cfgPath, *cfg); err != nil {
			t.Fatal(err)
		}
	}
	h.deps = cli.Env{
		Stdout: h.out,
		Stderr: h.errb,
		Getenv: func(k string) string {
			switch k {
			case "XDG_CONFIG_HOME":
				return dir
			case "CADRE_URL":
				return srv.URL
			}
			return ""
		},
		OpenBrowser: func(u string) error { h.opened = append(h.opened, u); return nil },
		Sleep:       func(time.Duration) {},
		HTTP:        srv.Client(),
		Version:     "1.2.3",
	}
	return h
}

func (h *harness) load(t *testing.T) config.Config {
	t.Helper()
	c, err := config.Load(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.WriteHeader(status)
	io.WriteString(w, body)
}

func loginServer(t *testing.T, expiresIn int, redeem func(n int, w http.ResponseWriter)) (*httptest.Server, *int32) {
	t.Helper()
	var polls int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/cli/logins":
			writeJSON(w, 201, `{"user_code":"ABCD-EFGH","device_code":"dev","verification_uri":"`+srv.URL+`/cli/authorize?code=ABCD-EFGH","expires_in":`+strconv.Itoa(expiresIn)+`,"interval":2}`)
		case "/api/cli/logins/redeem":
			redeem(int(atomic.AddInt32(&polls, 1)), w)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &polls
}

const expiredCopy = "The code expired. Run cadre login again."

func TestLoginSavesKeyAfterApproval(t *testing.T) {
	srv, _ := loginServer(t, 600, func(n int, w http.ResponseWriter) {
		if n < 3 {
			writeJSON(w, 202, `{"status":"pending"}`)
			return
		}
		writeJSON(w, 200, `{"api_key":"cadre_abc","email":"ada@colette.club","expires_at":"2027-01-01T00:00:00Z"}`)
	})
	h := newHarness(t, srv, nil)

	if code := cli.Run([]string{"login"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	uri := srv.URL + "/cli/authorize?code=ABCD-EFGH"
	want := "Opening " + uri + " in your browser.\nCheck that the page shows this code: ABCD-EFGH\nSigned in as ada@colette.club.\n"
	if h.out.String() != want {
		t.Errorf("stdout = %q, want %q", h.out, want)
	}
	if len(h.opened) != 1 || h.opened[0] != uri {
		t.Errorf("opened = %v", h.opened)
	}
	if got := h.load(t); got != (config.Config{URL: srv.URL, APIKey: "cadre_abc", Email: "ada@colette.club"}) {
		t.Errorf("config = %+v", got)
	}
	if strings.Contains(h.out.String()+h.errb.String(), "cadre_abc") {
		t.Error("key was printed")
	}
}

func TestLoginExpired(t *testing.T) {
	srv, _ := loginServer(t, 600, func(_ int, w http.ResponseWriter) { writeJSON(w, 410, `{"error":"expired"}`) })
	h := newHarness(t, srv, nil)

	if code := cli.Run([]string{"login"}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(h.errb.String(), expiredCopy) {
		t.Errorf("stderr = %q", h.errb)
	}
	if h.load(t).APIKey != "" {
		t.Error("config was written")
	}
}

func TestLoginNotFoundMeansExpired(t *testing.T) {
	srv, _ := loginServer(t, 600, func(_ int, w http.ResponseWriter) { writeJSON(w, 404, `{"error":"not_found"}`) })
	h := newHarness(t, srv, nil)

	if code := cli.Run([]string{"login"}, h.deps); code != 1 || !strings.Contains(h.errb.String(), expiredCopy) {
		t.Errorf("code = %d, stderr %q", code, h.errb)
	}
}

func TestLoginStopsAtExpiresIn(t *testing.T) {
	srv, polls := loginServer(t, 4, func(_ int, w http.ResponseWriter) { writeJSON(w, 202, `{"status":"pending"}`) })
	h := newHarness(t, srv, nil)

	if code := cli.Run([]string{"login"}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if *polls != 2 {
		t.Errorf("polls = %d, want 2", *polls)
	}
	if !strings.Contains(h.errb.String(), expiredCopy) {
		t.Errorf("stderr = %q", h.errb)
	}
}

func TestLoginWhenBrowserFails(t *testing.T) {
	srv, _ := loginServer(t, 600, func(_ int, w http.ResponseWriter) {
		writeJSON(w, 200, `{"api_key":"k","email":"ada@colette.club","expires_at":"x"}`)
	})
	h := newHarness(t, srv, nil)
	h.deps.OpenBrowser = func(string) error { return io.ErrClosedPipe }

	if code := cli.Run([]string{"login"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if !strings.Contains(h.out.String(), "/cli/authorize?code=ABCD-EFGH") {
		t.Errorf("stdout = %q", h.out)
	}
}

func meServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/me" || r.Header.Get("Authorization") != "Bearer cadre_abc" {
			t.Errorf("bad request %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		writeJSON(w, status, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWhoami(t *testing.T) {
	srv := meServer(t, 200, `{"email":"ada@colette.club"}`)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc", Email: "old@x"})

	if code := cli.Run([]string{"whoami"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Signed in to " + srv.URL + " as ada@colette.club.\n"; h.out.String() != want {
		t.Errorf("stdout = %q, want %q", h.out, want)
	}

	h = newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	if code := cli.Run([]string{"whoami", "--json"}, h.deps); code != 0 {
		t.Fatalf("code = %d", code)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(h.out.String()), &got); err != nil || got["email"] != "ada@colette.club" || got["url"] != srv.URL {
		t.Errorf("json = %q, %v", h.out, err)
	}
}

func TestWhoamiNotSignedIn(t *testing.T) {
	srv := meServer(t, 200, `{}`)
	h := newHarness(t, srv, nil)

	if code := cli.Run([]string{"whoami"}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(h.errb.String(), "Not signed in. Run cadre login.") {
		t.Errorf("stderr = %q", h.errb)
	}
}

func TestWhoamiInvalidKey(t *testing.T) {
	srv := meServer(t, 401, `{"error":"unauthorized"}`)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"whoami"}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(h.errb.String(), "Your key is no longer valid. Run cadre login.") {
		t.Errorf("stderr = %q", h.errb)
	}
}

func TestWhoamiNetworkErrorHidesKey(t *testing.T) {
	srv := meServer(t, 200, `{}`)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	srv.Close()

	if code := cli.Run([]string{"whoami"}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if h.errb.Len() == 0 || strings.Contains(h.errb.String(), "cadre_abc") {
		t.Errorf("stderr = %q", h.errb)
	}
}

func projectServer(t *testing.T, status int, body string, gotBody *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*gotBody = string(b)
		if r.URL.Path != "/api/projects" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer cadre_abc" {
			t.Errorf("bad request %s %s %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		writeJSON(w, status, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "project.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProjectCreate(t *testing.T) {
	var got string
	resp := `{"slug":"wish-reminders","milestones":2,"url":"https://cadre.example/"}`
	srv := projectServer(t, 201, resp, &got)
	file := writeFile(t, " {\"name\": \"Wish\"}\n")
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"project", "create", file}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Registered wish-reminders with 2 milestones: https://cadre.example/\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
	if got != " {\"name\": \"Wish\"}\n" {
		t.Errorf("body = %q", got)
	}

	h = newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	if code := cli.Run([]string{"--json", "project", "create", file}, h.deps); code != 0 || h.out.String() != resp {
		t.Errorf("code = %d, stdout = %q", code, h.out)
	}
}

func TestProjectCreateValidation(t *testing.T) {
	var got string
	srv := projectServer(t, 422, `{"errors":[{"path":"name","message":"is required"},{"path":"milestones.1.repos.0","message":"is invalid"}]}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"project", "create", writeFile(t, "{}")}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if want := "name: is required\nmilestones.1.repos.0: is invalid\n"; h.errb.String() != want {
		t.Errorf("stderr = %q, want %q", h.errb, want)
	}
}

func TestProjectCreateInvalidKey(t *testing.T) {
	var got string
	srv := projectServer(t, 401, `{"error":"unauthorized"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"project", "create", writeFile(t, "{}")}, h.deps); code != 1 ||
		!strings.Contains(h.errb.String(), "Your key is no longer valid. Run cadre login.") {
		t.Errorf("code = %d, stderr %q", code, h.errb)
	}
}

func TestProjectCreateNotSignedIn(t *testing.T) {
	var got string
	srv := projectServer(t, 201, `{}`, &got)
	h := newHarness(t, srv, nil)

	if code := cli.Run([]string{"project", "create", writeFile(t, "{}")}, h.deps); code != 1 ||
		!strings.Contains(h.errb.String(), "Not signed in. Run cadre login.") {
		t.Errorf("code = %d, stderr %q", code, h.errb)
	}
}

func TestProjectCreateMissingFile(t *testing.T) {
	var got string
	srv := projectServer(t, 201, `{}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"project", "create", "/nope/missing.json"}, h.deps); code != 1 ||
		!strings.Contains(h.errb.String(), "/nope/missing.json") {
		t.Errorf("code = %d, stderr %q", code, h.errb)
	}
}

func TestProjectCreateUsage(t *testing.T) {
	var got string
	srv := projectServer(t, 201, `{}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"project", "create"}, h.deps); code != 2 || !strings.Contains(h.errb.String(), "Usage: cadre") {
		t.Errorf("code = %d, stderr %q", code, h.errb)
	}
}
