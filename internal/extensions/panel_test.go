package extensions

// Panel and helper tests. The helper is a real command path, but every test
// here runs a throwaway script in a temporary directory (or a stub for the
// command factory), never sudo and never the installed helper.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// githubStub answers the releases endpoint with the given payload, or with an
// error status.
func githubStub(t *testing.T, status int, payload string) *GitHub {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/releases") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	gh := NewGitHub(srv.Client(), "", time.Minute)
	gh.BaseURL = srv.URL
	return gh
}

const newsPayload = `[
 {"tag_name":"news-v0.2.0","draft":false,"published_at":"2026-10-05T10:00:00Z",
  "assets":[{"name":"news-0.2.0.zip","browser_download_url":"https://example/news.zip"}]}
]`

// serviceFor builds a service over a temp apps dir with one configured app.
func serviceFor(t *testing.T, gh *GitHub) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	conf := []AppConf{{ID: "news", Repo: "gnacho/ocnews", Regex: regexp.MustCompile(`^news-[0-9.]+\.zip$`)}}
	return NewService(dir, "/nope", false, conf, gh), dir
}

func writeStamp(t *testing.T, dir, id, stamp string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, id), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id, ".ocapps-release"), []byte(stamp+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStamp(t *testing.T) {
	st := NewService(t.TempDir(), "/nope", false, nil, nil)
	if got := st.Stamp("news"); got != "" {
		t.Fatalf("Stamp of a missing app = %q, want empty", got)
	}
	writeStamp(t, st.AppsDir, "news", "release:news-v0.2.0")
	if got := st.Stamp("news"); got != "release:news-v0.2.0" {
		t.Fatalf("Stamp = %q, want the trimmed file content", got)
	}
}

func TestListStatuses(t *testing.T) {
	cases := []struct {
		name      string
		stamp     string
		wantStat  string
		wantError bool
	}{
		{"not installed", "", StatusUpdate, false},
		{"same release", "release:news-v0.2.0", StatusUpToDate, false},
		{"older release", "release:news-v0.1.0", StatusUpdate, false},
		{"local build", "local:my-build", StatusLocal, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, dir := serviceFor(t, githubStub(t, http.StatusOK, newsPayload))
			if tc.stamp != "" {
				writeStamp(t, dir, "news", tc.stamp)
			}
			apps := svc.List(context.Background())
			if len(apps) != 1 {
				t.Fatalf("apps = %+v, want one", apps)
			}
			if apps[0].Status != tc.wantStat {
				t.Fatalf("status = %q, want %q", apps[0].Status, tc.wantStat)
			}
			if apps[0].Latest != "news-v0.2.0" {
				t.Fatalf("latest = %q, want the release tag", apps[0].Latest)
			}
			if apps[0].Installed != tc.stamp {
				t.Fatalf("installed = %q, want %q", apps[0].Installed, tc.stamp)
			}
		})
	}
}

func TestListMarksOnlyTheFailingRow(t *testing.T) {
	svc, _ := serviceFor(t, githubStub(t, http.StatusInternalServerError, `{}`))

	apps := svc.List(context.Background())
	if len(apps) != 1 || apps[0].Status != StatusError {
		t.Fatalf("apps = %+v, want the row marked as error", apps)
	}
	if apps[0].Error == "" {
		t.Fatal("the failure reason must travel with the row")
	}
}

// stubCommand replaces the command factory and records what would have run.
type stubCommand struct {
	mu   sync.Mutex
	name string
	args []string
	out  string
	err  error
}

func (s *stubCommand) factory(_ context.Context, name string, args ...string) *exec.Cmd {
	s.mu.Lock()
	s.name = name
	s.args = args
	s.mu.Unlock()
	// A real (harmless) command that prints the recorded output, so the
	// service still gets a process with exit status.
	return exec.Command("sh", "-c", "printf '%s' '"+s.out+"'; exit "+exitCode(s.err))
}

func exitCode(err error) string {
	if err == nil {
		return "0"
	}
	return "3"
}

func withStubCommand(t *testing.T, stub *stubCommand) {
	t.Helper()
	previous := commandContext
	commandContext = stub.factory
	t.Cleanup(func() { commandContext = previous })
}

func TestUpdateBuildsTheSudoCommandLine(t *testing.T) {
	stub := &stubCommand{out: "updated\n"}
	withStubCommand(t, stub)
	svc, _ := serviceFor(t, githubStub(t, http.StatusOK, newsPayload))
	svc.UseSudo = true
	svc.Helper = "/usr/local/sbin/ocapps-ext"

	res, err := svc.Update(context.Background(), "news", true)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !res.OK || res.App != "news" {
		t.Fatalf("result = %+v", res)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.name != "sudo" {
		t.Fatalf("command = %q, want sudo", stub.name)
	}
	want := []string{"-n", "/usr/local/sbin/ocapps-ext", "update", "--force", "news"}
	if strings.Join(stub.args, " ") != strings.Join(want, " ") {
		t.Fatalf("args = %v, want %v", stub.args, want)
	}
}

func TestUpdateRunsTheHelperDirectly(t *testing.T) {
	stub := &stubCommand{out: "ok\n"}
	withStubCommand(t, stub)
	svc, _ := serviceFor(t, githubStub(t, http.StatusOK, newsPayload))
	svc.Helper = "/usr/local/sbin/ocapps-ext"

	if _, err := svc.Update(context.Background(), "news", false); err != nil {
		t.Fatalf("Update: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.name != "/usr/local/sbin/ocapps-ext" {
		t.Fatalf("command = %q, want the helper itself", stub.name)
	}
	if strings.Join(stub.args, " ") != "update news" {
		t.Fatalf("args = %v, want update news", stub.args)
	}
}

func TestUpdateRefusesUnknownApps(t *testing.T) {
	stub := &stubCommand{}
	withStubCommand(t, stub)
	svc, _ := serviceFor(t, githubStub(t, http.StatusOK, newsPayload))

	if _, err := svc.Update(context.Background(), "../etc/passwd", false); err == nil {
		t.Fatal("an app that is not configured must be refused")
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.name != "" {
		t.Fatalf("the helper ran with %q, an unknown id must not reach it", stub.name)
	}
}

func TestUpdateReportsTheFailure(t *testing.T) {
	stub := &stubCommand{out: "boom\n", err: exec.ErrNotFound}
	withStubCommand(t, stub)
	svc, _ := serviceFor(t, githubStub(t, http.StatusOK, newsPayload))

	res, err := svc.Update(context.Background(), "news", false)
	if err == nil {
		t.Fatal("a failing helper must return an error")
	}
	if res.OK {
		t.Fatal("a failing run must not be marked as ok")
	}
	if !strings.Contains(res.Output, "boom") {
		t.Fatalf("output = %q, want the helper output kept for the panel", res.Output)
	}
	if last := svc.Last(); last == nil || last.OK || last.App != "news" {
		t.Fatalf("last = %+v, want the failed run recorded", last)
	}
}

// TestUpdateUsesARealScript exercises the real exec path with a throwaway
// script: it prints its arguments and exits 0.
func TestUpdateUsesARealScript(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "args.txt")
	helper := filepath.Join(dir, "helper.sh")
	script := "#!/bin/sh\nprintf '%s' \"$*\" > " + out + "\necho done\n"
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	svc, _ := serviceFor(t, githubStub(t, http.StatusOK, newsPayload))
	svc.Helper = helper

	res, err := svc.Update(context.Background(), "news", true)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !res.OK || !strings.Contains(res.Output, "done") {
		t.Fatalf("result = %+v, want the output of the helper", res)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read the args the helper saw: %v", err)
	}
	if string(got) != "update --force news" {
		t.Fatalf("helper args = %q, want the update of the forced app", got)
	}
	if last := svc.Last(); last == nil || !last.OK || last.App != "news" {
		t.Fatalf("last = %+v, want the successful run", last)
	}
}

func TestUpdateRefusesToRunTwice(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "slow.sh")
	// The script holds the update open long enough for the second call.
	script := "#!/bin/sh\nsleep 0.5\necho done\n"
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	svc, _ := serviceFor(t, githubStub(t, http.StatusOK, newsPayload))
	svc.Helper = helper

	done := make(chan error, 1)
	go func() {
		_, err := svc.Update(context.Background(), "news", false)
		done <- err
	}()

	// Wait until the first run is visible, then the second must be refused.
	deadline := time.Now().Add(5 * time.Second)
	for svc.Running() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if svc.Running() != "news" {
		t.Fatal("the running app must be visible while the helper works")
	}
	if _, err := svc.Update(context.Background(), "news", false); err == nil ||
		!strings.Contains(err.Error(), "already running") {
		t.Fatalf("second update = %v, want it refused", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("first update: %v", err)
	}
	if svc.Running() != "" {
		t.Fatalf("running = %q after the update, want empty", svc.Running())
	}
}

func TestUpdateHonoursTheTimeout(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "slow.sh")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nsleep 5\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	svc, _ := serviceFor(t, githubStub(t, http.StatusOK, newsPayload))
	svc.Helper = helper
	svc.Timeout = 100 * time.Millisecond

	if _, err := svc.Update(context.Background(), "news", false); err == nil {
		t.Fatal("an update that overruns its budget must fail")
	}
}

func TestLastWithoutRuns(t *testing.T) {
	svc, _ := serviceFor(t, nil)
	if svc.Last() != nil {
		t.Fatal("Last must be nil before any run")
	}
	if svc.Running() != "" {
		t.Fatal("Running must be empty before any run")
	}
}

func TestErrString(t *testing.T) {
	if got := errString(nil); got != "" {
		t.Fatalf("errString(nil) = %q, want empty", got)
	}
	if got := errString(os.ErrNotExist); got != os.ErrNotExist.Error() {
		t.Fatalf("errString = %q", got)
	}
}

func TestRegisterWithoutTokenMountsNothing(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, NewService(t.TempDir(), "/nope", false, nil, nil), "")
	if _, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, "/ocapps-admin/", nil)); pattern != "" {
		t.Fatalf("pattern = %q, the panel must not be registered", pattern)
	}
}

// panelRequest sends a request to the registered panel.
func panelRequest(t *testing.T, mux *http.ServeMux, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("X-Ocapps-Admin-Token", token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestPanelEndpoints(t *testing.T) {
	stub := &stubCommand{out: "ok\n"}
	withStubCommand(t, stub)
	svc, dir := serviceFor(t, githubStub(t, http.StatusOK, newsPayload))
	writeStamp(t, dir, "news", "release:news-v0.1.0")

	mux := http.NewServeMux()
	Register(mux, svc, "admin-token")

	// The HTML is public but must not be cached.
	rec := panelRequest(t, mux, http.MethodGet, "/ocapps-admin/", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("panel = %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}

	// The API needs the token.
	if rec := panelRequest(t, mux, http.MethodGet, "/ocapps-admin/api/extensions", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("list without token = %d, want 401", rec.Code)
	}
	if rec := panelRequest(t, mux, http.MethodGet, "/ocapps-admin/api/extensions", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("list with a wrong token = %d, want 401", rec.Code)
	}

	rec = panelRequest(t, mux, http.MethodGet, "/ocapps-admin/api/extensions", "admin-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	var list struct {
		Apps    []App      `json:"apps"`
		Last    *RunResult `json:"last"`
		Running string     `json:"running"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v (%s)", err, rec.Body.String())
	}
	if len(list.Apps) != 1 || list.Apps[0].Status != StatusUpdate {
		t.Fatalf("apps = %+v, want the outdated one", list.Apps)
	}
	if list.Last != nil || list.Running != "" {
		t.Fatalf("last/running = %+v/%q, want both empty before any update", list.Last, list.Running)
	}

	// Update through the panel.
	rec = panelRequest(t, mux, http.MethodPost, "/ocapps-admin/api/extensions/news/update?force=1", "admin-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d (%s)", rec.Code, rec.Body.String())
	}
	var upd struct {
		Result RunResult `json:"result"`
		Error  string    `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &upd); err != nil {
		t.Fatalf("decode update: %v", err)
	}
	if !upd.Result.OK || upd.Result.App != "news" || upd.Error != "" {
		t.Fatalf("update = %+v, want a successful run", upd)
	}

	// An unknown app answers 502 with the reason, never running anything.
	stub.mu.Lock()
	stub.name = ""
	stub.mu.Unlock()
	rec = panelRequest(t, mux, http.MethodPost, "/ocapps-admin/api/extensions/ghost/update", "admin-token")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("unknown app = %d, want 502", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &upd); err != nil {
		t.Fatalf("decode unknown: %v", err)
	}
	if !strings.Contains(upd.Error, "unknown app") {
		t.Fatalf("error = %q, want the reason", upd.Error)
	}
}
