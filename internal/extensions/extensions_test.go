package extensions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

func TestLoadConf(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "extensions.conf")
	body := `# comment
news|gnacho/ocnews|^news-[0-9.]+\.zip$

agenda|gnacho/opencloud-agenda|^agenda-[0-9.]+\.zip$
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	conf, err := LoadConf(path)
	if err != nil {
		t.Fatalf("LoadConf: %v", err)
	}
	if len(conf) != 2 {
		t.Fatalf("got %d entries, want 2", len(conf))
	}
	if conf[0].ID != "news" || conf[1].Repo != "gnacho/opencloud-agenda" {
		t.Fatalf("unexpected parse: %+v", conf)
	}
	if !conf[0].Regex.MatchString("news-0.2.0.zip") {
		t.Fatal("regex should match the release asset")
	}
}

func TestLoadConfRejectsBrokenLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "extensions.conf")
	for _, body := range []string{"news|gnacho/ocnews\n", "news|gnacho/ocnews|(\n", "# only comments\n"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConf(path); err == nil {
			t.Fatalf("expected an error for %q", body)
		}
	}
}

func TestLatestPicksNewestMatchingAsset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"tag_name":"v0.1.9","draft":false,"published_at":"2026-10-07T10:00:00Z",
			 "assets":[{"name":"ocnews_0.1.9_linux_amd64.tar.gz","browser_download_url":"https://example/binary.tgz"}]},
			{"tag_name":"news-v0.2.0","draft":false,"published_at":"2026-10-05T10:00:00Z",
			 "assets":[{"name":"news-0.2.0.zip","browser_download_url":"https://example/news.zip"}]},
			{"tag_name":"news-v0.1.9","draft":true,"published_at":"2026-10-09T10:00:00Z",
			 "assets":[{"name":"news-0.1.9.zip","browser_download_url":"https://example/draft.zip"}]}
		]`))
	}))
	defer srv.Close()

	gh := NewGitHub(srv.Client(), "", time.Minute)
	gh.BaseURL = srv.URL

	rel, err := gh.Latest(context.Background(), "gnacho/ocnews", regexp.MustCompile(`^news-[0-9.]+\.zip$`))
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rel.Tag != "news-v0.2.0" || rel.AssetURL != "https://example/news.zip" {
		t.Fatalf("unexpected release: %+v", rel)
	}
}

func TestLatestWithoutMatchingAsset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"tag_name":"v1","draft":false,"published_at":"2026-01-01T00:00:00Z","assets":[]}]`))
	}))
	defer srv.Close()

	gh := NewGitHub(srv.Client(), "", time.Minute)
	gh.BaseURL = srv.URL
	if _, err := gh.Latest(context.Background(), "x/y", regexp.MustCompile(`^app-.*\.zip$`)); err == nil {
		t.Fatal("expected an error when no asset matches")
	}
}

func TestListStatus(t *testing.T) {
	apps := t.TempDir()
	writeStamp := func(id, stamp string) {
		dir := filepath.Join(apps, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".ocapps-release"), []byte(stamp+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeStamp("news", "release:v0.2.0")
	writeStamp("notes", "local:2026-11-08")
	// ocphotos is not installed

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"tag_name":"v0.2.0","draft":false,"published_at":"2026-10-05T10:00:00Z",
			"assets":[{"name":"news-0.2.0.zip","browser_download_url":"https://example/a.zip"}]}]`))
	}))
	defer srv.Close()

	gh := NewGitHub(srv.Client(), "", time.Minute)
	gh.BaseURL = srv.URL
	conf := []AppConf{
		{ID: "news", Repo: "gnacho/ocnews", Regex: regexp.MustCompile(`\.zip$`)},
		{ID: "notes", Repo: "gnacho/ocnotes", Regex: regexp.MustCompile(`\.zip$`)},
		{ID: "ocphotos", Repo: "gnacho/ocphotos", Regex: regexp.MustCompile(`\.zip$`)},
	}
	svc := NewService(apps, "/usr/local/sbin/ocapps-ext", false, conf, gh)

	got := map[string]string{}
	for _, a := range svc.List(context.Background()) {
		got[a.ID] = a.Status
	}
	if got["news"] != StatusUpToDate {
		t.Errorf("news = %s, want %s", got["news"], StatusUpToDate)
	}
	if got["notes"] != StatusLocal {
		t.Errorf("notes = %s, want %s", got["notes"], StatusLocal)
	}
	if got["ocphotos"] != StatusUpdate {
		t.Errorf("ocphotos = %s, want %s", got["ocphotos"], StatusUpdate)
	}
}

func TestUpdateRejectsUnknownApp(t *testing.T) {
	svc := NewService(t.TempDir(), "/bin/true", false, []AppConf{{ID: "news"}}, NewGitHub(nil, "", time.Minute))
	if _, err := svc.Update(context.Background(), "../etc", false); err == nil {
		t.Fatal("expected an error for an app outside the configuration")
	}
}

func TestRegisterWithoutTokenDoesNothing(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, NewService(t.TempDir(), "/bin/true", false, nil, nil), "")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ocapps-admin/api/extensions", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when the token is empty", rec.Code)
	}
}

func TestAuthRejectsWrongToken(t *testing.T) {
	apps := t.TempDir()
	svc := NewService(apps, "/bin/true", false, []AppConf{}, NewGitHub(nil, "", time.Minute))
	mux := http.NewServeMux()
	Register(mux, svc, "secret")

	req := httptest.NewRequest(http.MethodGet, "/ocapps-admin/api/extensions", nil)
	req.Header.Set("X-Ocapps-Admin-Token", "nope")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
