package cli_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/djeusette/cadre_cli/internal/cli"
	"github.com/djeusette/cadre_cli/internal/config"
)

type statusRequest struct{ method, path, auth string }

// statusServer answers every request with status and body, and records the last one.
func statusServer(t *testing.T, status int, body string, got *statusRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = statusRequest{r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization")}
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, status, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const statusBody = `{
  "can_deploy": [
    {"name":"colette-api","ships":[
      {"number":418,"url":"u","project":"Wish reminders","slug":"wish-reminders","milestone":"2/4","held":false,"blocked_by":null},
      {"number":399,"url":"u","project":null,"slug":null,"milestone":null,"held":false,"blocked_by":null}],
     "unblocks":[{"name":"club-mobile","number":91}],"last_deployed_at":"2026-10-20T23:30:00Z"},
    {"name":"colette-agents","ships":[
      {"number":61,"url":"u","project":"Sentry alerts to Linear","slug":"sentry","milestone":"2/2","held":false,"blocked_by":null}],
     "unblocks":[],"last_deployed_at":null}
  ],
  "nothing_to_ship":["club-mobile"],
  "not_tracked":["colette-site"],
  "projects":[
    {"slug":"wish-reminders","name":"Wish reminders","deployed":1,"total":4,
     "current":{"number":2,"title":"Set a reminder in the app","state":"can_deploy","state_label":"Can deploy"},
     "next_step":{"text":"Deploy colette-api","meta":"","pull_requests":[]}},
    {"slug":"gift-cards","name":"Gift cards","deployed":4,"total":6,
     "current":{"number":5,"title":"Send a gift by email","state":"in_review","state_label":"In review"},
     "next_step":{"text":"Review club-mobile #95","meta":"","pull_requests":[]}}
  ]
}`

func TestStatusPrintsTheQueueAndProjects(t *testing.T) {
	var got statusRequest
	srv := statusServer(t, 200, statusBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"status"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	want := "Can deploy now\n" +
		"  colette-api      ships #418 Wish reminders 2/4, #399 not linked\n" +
		"                   unblocks club-mobile #91 · last deployed Oct 20\n" +
		"  colette-agents   ships #61 Sentry alerts to Linear 2/2\n" +
		"Nothing to ship: club-mobile\n" +
		"Not tracked: colette-site\n" +
		"\n" +
		"Projects (2 active)\n" +
		"  Wish reminders   1 of 4  2 · Set a reminder in the app  Can deploy  Deploy colette-api\n" +
		"  Gift cards       4 of 6  5 · Send a gift by email       In review   Review club-mobile #95\n"
	if h.out.String() != want {
		t.Errorf("stdout = %q\nwant     %q", h.out, want)
	}
	if got.method != "GET" || got.path != "/api/status" || got.auth != "Bearer cadre_abc" {
		t.Errorf("request = %+v", got)
	}
}

func TestStatusPadsByCharactersNotBytes(t *testing.T) {
	var got statusRequest
	body := `{"can_deploy":[],"nothing_to_ship":[],"not_tracked":[],"projects":[
	  {"slug":"a","name":"Ünï","deployed":1,"total":2,"current":{"number":1,"title":"Ü","state":"x","state_label":"Go"},"next_step":{"text":"A","meta":"","pull_requests":[]}},
	  {"slug":"b","name":"Abc","deployed":1,"total":2,"current":{"number":1,"title":"Ab","state":"x","state_label":"Go"},"next_step":{"text":"B","meta":"","pull_requests":[]}}]}`
	srv := statusServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"status"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	want := "Can deploy now: nothing\n\nProjects (2 active)\n" +
		"  Ünï   1 of 2  1 · Ü   Go  A\n" +
		"  Abc   1 of 2  1 · Ab  Go  B\n"
	if h.out.String() != want {
		t.Errorf("stdout = %q\nwant     %q", h.out, want)
	}
}

func TestStatusNamesHeldAndBlockedRows(t *testing.T) {
	var got statusRequest
	body := `{"can_deploy":[
	  {"name":"colette-api","ships":[
	    {"number":37,"url":"u","project":"Wish reminders","slug":"w","milestone":"2/4","held":false,"blocked_by":{"name":"api","number":36}},
	    {"number":40,"url":"u","project":"Gift cards","slug":"g","milestone":"5/6","held":true,"blocked_by":null},
	    {"number":41,"url":"u","project":"Gift cards","slug":"g","milestone":"5/6","held":false,"blocked_by":{"name":"api","number":null}}],
	   "unblocks":[{"name":"club-mobile","number":null}],"last_deployed_at":null}],
	  "nothing_to_ship":[],"not_tracked":[],"projects":[]}`
	srv := statusServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"status"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	want := "Can deploy now\n" +
		"  colette-api   ships #37 Wish reminders 2/4 blocked by api #36, #40 Gift cards 5/6 held, #41 Gift cards 5/6 blocked by api\n" +
		"                unblocks club-mobile\n" +
		"\nProjects (0 active)\n"
	if h.out.String() != want {
		t.Errorf("stdout = %q\nwant     %q", h.out, want)
	}
}

func TestStatusWithNothing(t *testing.T) {
	var got statusRequest
	srv := statusServer(t, 200, `{"can_deploy":[],"nothing_to_ship":[],"not_tracked":[],"projects":[]}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"status"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Can deploy now: nothing\n\nProjects (0 active)\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
}

func TestStatusJSONPrintsTheServersAnswer(t *testing.T) {
	for name, c := range map[string]struct {
		args []string
		path string
	}{
		"queue":   {[]string{"status", "--json"}, "/api/status"},
		"project": {[]string{"status", "wish-reminders", "--json"}, "/api/projects/wish-reminders/status"},
	} {
		var got statusRequest
		srv := statusServer(t, 200, statusBody, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

		if code := cli.Run(c.args, h.deps); code != 0 {
			t.Fatalf("%s: code = %d, stderr %q", name, code, h.errb)
		}
		if want := statusBody; h.out.String() != want {
			t.Errorf("%s: stdout = %q", name, h.out)
		}
		if got.path != c.path {
			t.Errorf("%s: path = %q, want %q", name, got.path, c.path)
		}
	}
}

const projectStatusBody = `{
  "slug":"wish-reminders","name":"Wish reminders","group":"active","deployed":1,"total":4,
  "next_step":{"text":"Deploy colette-api","meta":"ships #418; then club-mobile #91 needs a second approval","pull_requests":[]},
  "milestones":[
    {"number":1,"title":"Sign up for reminders","state":"deployed","state_label":"Deployed","current":false,
     "steps":[{"repo":"colette-api","app":null,"pull_requests":[{"number":410,"url":"u","title":"t","status":"deployed","status_label":"Deployed"}]}],
     "next_step":null},
    {"number":2,"title":"Set a reminder in the app","state":"can_deploy","state_label":"Can deploy","current":true,
     "steps":[
       {"repo":"colette-api","app":null,"pull_requests":[{"number":418,"url":"u","title":"t","status":"merged","status_label":"Merged"}]},
       {"repo":"club-mobile","app":null,"pull_requests":[]}],
     "next_step":{"text":"Deploy colette-api","meta":"ships #418","pull_requests":[]}}
  ]}`

func TestProjectStatusPrintsMilestones(t *testing.T) {
	var got statusRequest
	srv := statusServer(t, 200, projectStatusBody, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"status", "wish-reminders"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	want := "Wish reminders · 1 of 4 deployed\n" +
		"Next: Deploy colette-api · ships #418; then club-mobile #91 needs a second approval\n" +
		"\n" +
		"1 · Sign up for reminders  Deployed\n" +
		"    colette-api  #410  Deployed\n" +
		"2 · Set a reminder in the app  Can deploy\n" +
		"    colette-api  #418  Merged\n" +
		"    club-mobile  no PR yet\n" +
		"    Next: Deploy colette-api · ships #418\n"
	if h.out.String() != want {
		t.Errorf("stdout = %q\nwant     %q", h.out, want)
	}
	if got.method != "GET" || got.path != "/api/projects/wish-reminders/status" {
		t.Errorf("request = %+v", got)
	}
}

func TestProjectStatusNamesAppsGroupsAndEmptyMeta(t *testing.T) {
	var got statusRequest
	body := `{"slug":"w","name":"Wish","group":"finished","deployed":1,"total":1,"next_step":null,
	  "milestones":[{"number":1,"title":"One","state":"deployed","state_label":"Deployed","current":false,
	    "steps":[{"repo":"colette-mobile","app":"club-mobile","pull_requests":[
	      {"number":5,"url":"u","title":"t","status":"merged","status_label":"Merged"},
	      {"number":6,"url":"u","title":"t","status":"deployed","status_label":"Deployed"}]}],
	    "next_step":{"text":"Deploy it","meta":"","pull_requests":[]}}]}`
	srv := statusServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"status", "w"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	want := "Wish · 1 of 1 deployed · Finished\n" +
		"\n" +
		"1 · One  Deployed\n" +
		"    colette-mobile / club-mobile  #5  Merged\n" +
		"    colette-mobile / club-mobile  #6  Deployed\n" +
		"    Next: Deploy it\n"
	if h.out.String() != want {
		t.Errorf("stdout = %q\nwant     %q", h.out, want)
	}

	archived := `{"slug":"w","name":"Wish","group":"archived","deployed":0,"total":0,"next_step":null,"milestones":[]}`
	srv = statusServer(t, 200, archived, &got)
	h = newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})
	if code := cli.Run([]string{"status", "w"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	if want := "Wish · 0 of 0 deployed · Archived\n"; h.out.String() != want {
		t.Errorf("stdout = %q", h.out)
	}
}

func TestProjectStatusUnknownProject(t *testing.T) {
	var got statusRequest
	srv := statusServer(t, 404, `{"error":"not_found"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"status", "Wish/1"}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if want := "No project Wish/1 on " + srv.URL + ".\n"; h.errb.String() != want {
		t.Errorf("stderr = %q", h.errb)
	}
	if got.path != "/api/projects/Wish%2F1/status" {
		t.Errorf("path = %q", got.path)
	}
}

func TestStatusNotSignedIn(t *testing.T) {
	var got statusRequest
	srv := statusServer(t, 200, `{}`, &got)
	for _, args := range [][]string{{"status"}, {"status", "wish-reminders"}} {
		h := newHarness(t, srv, &config.Config{})
		if code := cli.Run(args, h.deps); code != 1 {
			t.Fatalf("%q: code = %d", args, code)
		}
		if want := "Not signed in. Run cadre login --url <url>.\n"; h.errb.String() != want {
			t.Errorf("%q: stderr = %q", args, h.errb)
		}
	}
	if got.path != "" {
		t.Errorf("the server was called: %+v", got)
	}
}

func TestStatusAnOlderServerIsAnError(t *testing.T) {
	var got statusRequest
	srv := statusServer(t, 404, `{"error":"not_found"}`, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"status"}, h.deps); code != 1 {
		t.Fatalf("code = %d", code)
	}
	want := "Could not complete the request to " + srv.URL + ": the server answered with an unexpected status 404\n"
	if h.errb.String() != want || h.out.Len() != 0 {
		t.Errorf("stdout = %q, stderr = %q\nwant stderr %q", h.out, h.errb, want)
	}
}

func TestStatusLastDeployedIsADayInUTCWhateverTheZone(t *testing.T) {
	ahead := time.FixedZone("ahead", 3*60*60)
	previous := time.Local
	time.Local = ahead
	t.Cleanup(func() { time.Local = previous })

	for _, sent := range []string{"2026-10-20T23:30:00Z", "2026-10-21T01:30:00+02:00"} {
		var got statusRequest
		body := `{"can_deploy":[{"name":"colette-api","ships":[
		  {"number":5,"url":"u","project":null,"slug":null,"milestone":null,"held":false,"blocked_by":null}],
		  "unblocks":[],"last_deployed_at":"` + sent + `"}],"nothing_to_ship":[],"not_tracked":[],"projects":[]}`
		srv := statusServer(t, 200, body, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

		if code := cli.Run([]string{"status"}, h.deps); code != 0 {
			t.Fatalf("code = %d, stderr %q", code, h.errb)
		}
		if want := "                last deployed Oct 20\n"; !strings.Contains(h.out.String(), want) {
			t.Errorf("sent %s: stdout = %q, want it to contain %q", sent, h.out, want)
		}
	}
}

func TestStatusHeldAndBlockedTogether(t *testing.T) {
	var got statusRequest
	body := `{"can_deploy":[{"name":"colette-api","ships":[
	  {"number":37,"url":"u","project":"Wish reminders","slug":"w","milestone":"2/4","held":true,"blocked_by":{"name":"api","number":36}}],
	  "unblocks":[],"last_deployed_at":null}],"nothing_to_ship":[],"not_tracked":[],"projects":[]}`
	srv := statusServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"status"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	want := "Can deploy now\n  colette-api   ships #37 Wish reminders 2/4 blocked by api #36 held\n\nProjects (0 active)\n"
	if h.out.String() != want {
		t.Errorf("stdout = %q\nwant     %q", h.out, want)
	}
}

func TestStatusCardWithOnlyALastDeploy(t *testing.T) {
	var got statusRequest
	body := `{"can_deploy":[{"name":"colette-api","ships":[
	  {"number":5,"url":"u","project":null,"slug":null,"milestone":null,"held":false,"blocked_by":null}],
	  "unblocks":[],"last_deployed_at":"2026-10-20T23:30:00Z"}],"nothing_to_ship":[],"not_tracked":[],"projects":[]}`
	srv := statusServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"status"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	want := "Can deploy now\n  colette-api   ships #5 not linked\n                last deployed Oct 20\n\nProjects (0 active)\n"
	if h.out.String() != want {
		t.Errorf("stdout = %q\nwant     %q", h.out, want)
	}
}

func TestStatusProjectWithNoCurrentMilestoneOrNextStep(t *testing.T) {
	var got statusRequest
	body := `{"can_deploy":[],"nothing_to_ship":[],"not_tracked":[],"projects":[
	  {"slug":"a","name":"Done","deployed":2,"total":2,"current":null,"next_step":null},
	  {"slug":"b","name":"Open","deployed":1,"total":2,"current":{"number":2,"title":"Two","state":"x","state_label":"Go"},"next_step":null},
	  {"slug":"c","name":"Odd","deployed":0,"total":2,"current":null,"next_step":{"text":"Wait","meta":"","pull_requests":[]}}]}`
	srv := statusServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"status"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	want := "Can deploy now: nothing\n\nProjects (3 active)\n" +
		"  Done   2 of 2  -        -   -\n" +
		"  Open   1 of 2  2 · Two  Go  -\n" +
		"  Odd    0 of 2  -        -   Wait\n"
	if h.out.String() != want {
		t.Errorf("stdout = %q\nwant     %q", h.out, want)
	}
}

func TestProjectStatusStepWithAnAppAndNoPR(t *testing.T) {
	var got statusRequest
	body := `{"slug":"w","name":"Wish","group":"active","deployed":0,"total":1,"next_step":null,
	  "milestones":[{"number":1,"title":"One","state":"open","state_label":"Open","current":true,
	    "steps":[{"repo":"colette-mobile","app":"club-mobile","pull_requests":[]}],"next_step":null}]}`
	srv := statusServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"status", "w"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	want := "Wish · 0 of 1 deployed\n\n1 · One  Open\n    colette-mobile / club-mobile  no PR yet\n"
	if h.out.String() != want {
		t.Errorf("stdout = %q\nwant     %q", h.out, want)
	}
}

func TestStatusRefusedKeyAndServerError(t *testing.T) {
	for name, c := range map[string]struct {
		args   []string
		status int
		want   func(url string) string
	}{
		"401 on a project": {[]string{"status", "wish-reminders"}, 401, func(u string) string {
			return "Your key for " + u + " is no longer valid. Run cadre login --url " + u + ".\n"
		}},
		"500 on the queue": {[]string{"status"}, 500, func(u string) string {
			return "Could not complete the request to " + u + ": the server answered with an unexpected status 500\n"
		}},
	} {
		var got statusRequest
		srv := statusServer(t, c.status, `{"error":"x"}`, &got)
		h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

		if code := cli.Run(c.args, h.deps); code != 1 {
			t.Fatalf("%s: code = %d", name, code)
		}
		if h.errb.String() != c.want(srv.URL) || h.out.Len() != 0 {
			t.Errorf("%s: stdout = %q, stderr = %q", name, h.out, h.errb)
		}
	}
}

func TestStatusPadsQueueNamesByCharactersNotBytes(t *testing.T) {
	var got statusRequest
	body := `{"can_deploy":[
	  {"name":"café","ships":[{"number":1,"url":"u","project":null,"slug":null,"milestone":null,"held":false,"blocked_by":null}],
	   "unblocks":[],"last_deployed_at":null},
	  {"name":"api","ships":[{"number":2,"url":"u","project":null,"slug":null,"milestone":null,"held":false,"blocked_by":null}],
	   "unblocks":[],"last_deployed_at":null}],
	  "nothing_to_ship":[],"not_tracked":[],"projects":[]}`
	srv := statusServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"status"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	want := "Can deploy now\n" +
		"  café   ships #1 not linked\n" +
		"  api    ships #2 not linked\n" +
		"\nProjects (0 active)\n"
	if h.out.String() != want {
		t.Errorf("stdout = %q\nwant     %q", h.out, want)
	}
}

func TestStatusAPRWithOnlyOneOfProjectAndMilestoneIsNotLinked(t *testing.T) {
	var got statusRequest
	body := `{"can_deploy":[
	  {"name":"colette-api","ships":[
	    {"number":5,"url":"u","project":null,"slug":null,"milestone":"2/4","held":false,"blocked_by":null},
	    {"number":6,"url":"u","project":"Gift cards","slug":"g","milestone":null,"held":false,"blocked_by":null}],
	   "unblocks":[],"last_deployed_at":null}],
	  "nothing_to_ship":[],"not_tracked":[],"projects":[]}`
	srv := statusServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"status"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	want := "Can deploy now\n" +
		"  colette-api   ships #5 not linked, #6 not linked\n" +
		"\nProjects (0 active)\n"
	if h.out.String() != want {
		t.Errorf("stdout = %q\nwant     %q", h.out, want)
	}
}

func TestStatusProjectsTableShowsTheNextStepTextWithoutItsMeta(t *testing.T) {
	var got statusRequest
	body := `{"can_deploy":[],"nothing_to_ship":[],"not_tracked":[],"projects":[
	  {"slug":"a","name":"Abc","deployed":1,"total":2,"current":{"number":1,"title":"Ab","state":"x","state_label":"Go"},
	   "next_step":{"text":"Deploy api","meta":"ships #418","pull_requests":[]}}]}`
	srv := statusServer(t, 200, body, &got)
	h := newHarness(t, srv, &config.Config{APIKey: "cadre_abc"})

	if code := cli.Run([]string{"status"}, h.deps); code != 0 {
		t.Fatalf("code = %d, stderr %q", code, h.errb)
	}
	want := "Can deploy now: nothing\n\nProjects (1 active)\n" +
		"  Abc   1 of 2  1 · Ab  Go  Deploy api\n"
	if h.out.String() != want {
		t.Errorf("stdout = %q\nwant     %q", h.out, want)
	}
}
