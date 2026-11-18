package photos

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gnacho/ocapps/internal/common/auth"
	"github.com/gnacho/ocapps/internal/common/config"
)

// fakeDrivesOpenCloud is fakeOpenCloud with several drives (issue #145): the
// personal one plus a project space, both with the photos under "Fotos".
func fakeDrivesOpenCloud(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/graph/v1.0/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "user-1", "onPremisesSamAccountName": "admin"})
		case r.URL.Path == "/graph/v1.0/me/drives":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{
				{"id": "p1", "name": "Personal", "driveType": "personal",
					"root": map[string]any{"webDavUrl": srv.URL + "/dav/spaces/p"}},
				{"id": "k1", "name": "Familia", "driveType": "project",
					"root": map[string]any{"webDavUrl": srv.URL + "/dav/spaces/k"}},
			}})
		case r.Method == "PROPFIND":
			var body string
			switch r.URL.Path {
			case "/dav/spaces/p/Fotos/", "/dav/spaces/p/Fotos":
				body = multistatusXML("/dav/spaces/p/Fotos/", "/dav/spaces/p/Fotos/a.jpg")
			case "/dav/spaces/k/Fotos/", "/dav/spaces/k/Fotos":
				body = multistatusXML("/dav/spaces/k/Fotos/", "/dav/spaces/k/Fotos/b.jpg")
			default:
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = fmt.Fprint(w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func multistatusXML(self string, entries ...string) string {
	write := func(href string) string {
		return `<d:response><d:href>` + href + `</d:href><d:propstat><d:prop><d:resourcetype></d:resourcetype>` +
			`<d:getetag>"e-` + href + `"</d:getetag><d:getlastmodified>Mon, 02 Jan 2006 15:04:05 GMT</d:getlastmodified>` +
			`<d:getcontentlength>100</d:getcontentlength></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`
	}
	out := `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">` + write(self)
	for _, e := range entries {
		out += write(e)
	}
	return out + `</d:multistatus>`
}

func newSpacesModule(t *testing.T, srv *httptest.Server, pcfg config.PhotosConfig) *Module {
	t.Helper()
	if pcfg.ScanEvery == 0 {
		pcfg.ScanEvery = time.Hour
	}
	m, err := New(testConfig(config.Common{OpenCloudURL: srv.URL, PhotosDataDir: t.TempDir()}, pcfg), slog.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestResolveSpacesDefaultsToEveryVisibleDrive: with no configuration the scan
// walks the personal space AND the project spaces/shares the owner can see
// (issue #145).
func TestResolveSpacesDefaultsToEveryVisibleDrive(t *testing.T) {
	srv := fakeDrivesOpenCloud(t)
	m := newSpacesModule(t, srv, config.PhotosConfig{ScanRoot: "Fotos"})
	ctx := context.Background()
	m.reg.Touch(ctx, &auth.User{ID: "user-1"}, auth.Credential{Bearer: "tok"})

	spaces, err := m.reg.resolveSpaces(ctx, m.reg.get("user-1"), "user-1")
	if err != nil {
		t.Fatalf("resolveSpaces: %v", err)
	}
	if len(spaces) != 2 {
		t.Fatalf("spaces = %+v, want 2", spaces)
	}
	if spaces[0].ID != "p1" || spaces[0].DriveType != "personal" || spaces[1].ID != "k1" {
		t.Errorf("spaces = %+v", spaces)
	}
	// SpacePrefixOf resolves the path of each one (folders handler)
	if got := m.reg.SpacePrefixOf(ctx, "user-1", "k1"); got != "/dav/spaces/k" {
		t.Errorf("SpacePrefixOf(k1) = %q", got)
	}
	if got := m.reg.SpacePrefixOf(ctx, "user-1", "nope"); got != "" {
		t.Errorf("SpacePrefixOf(nope) = %q, want empty", got)
	}
	// The personal one is what the legacy single-space callers get
	if got, err := m.reg.resolveSpace(ctx, m.reg.get("user-1"), "user-1"); err != nil || got != srv.URL+"/dav/spaces/p" {
		t.Errorf("resolveSpace = %q %v", got, err)
	}
}

// TestResolveSpacesConfigured: the filter still works, with drive ids and with
// the "personal" keyword, and an id that is not visible is an error.
func TestResolveSpacesConfigured(t *testing.T) {
	srv := fakeDrivesOpenCloud(t)
	ctx := context.Background()

	byID := newSpacesModule(t, srv, config.PhotosConfig{ScanRoot: "Fotos", ScanSpaces: []string{"k1"}})
	byID.reg.Touch(ctx, &auth.User{ID: "user-1"}, auth.Credential{Bearer: "tok"})
	spaces, err := byID.reg.resolveSpaces(ctx, byID.reg.get("user-1"), "user-1")
	if err != nil || len(spaces) != 1 || spaces[0].ID != "k1" {
		t.Fatalf("by id: %+v %v", spaces, err)
	}

	personal := newSpacesModule(t, srv, config.PhotosConfig{ScanRoot: "Fotos", ScanSpaces: []string{"personal"}})
	personal.reg.Touch(ctx, &auth.User{ID: "user-1"}, auth.Credential{Bearer: "tok"})
	spaces, err = personal.reg.resolveSpaces(ctx, personal.reg.get("user-1"), "user-1")
	if err != nil || len(spaces) != 1 || spaces[0].ID != "p1" {
		t.Fatalf("personal: %+v %v", spaces, err)
	}
}

// TestScanWalksEverySpace: the end-to-end path of the scheduler: resolve every
// space, scan it, and leave in the index the space of each asset.
func TestScanWalksEverySpace(t *testing.T) {
	srv := fakeDrivesOpenCloud(t)
	m := newSpacesModule(t, srv, config.PhotosConfig{ScanRoot: "Fotos"})
	ctx := context.Background()
	m.reg.Touch(ctx, &auth.User{ID: "user-1"}, auth.Credential{Bearer: "tok"})

	m.sched.scanOne(ctx, "user-1")

	all, err := m.st.AssetsPage(ctx, "user-1", 1<<62-1, 1<<62-1, 10, false, false, "", "")
	if err != nil || len(all) != 2 {
		t.Fatalf("assets = %d, err=%v", len(all), err)
	}
	names := map[string]string{}
	for _, a := range all {
		names[a.SpaceID] = a.SpaceName
	}
	if names["p1"] != "Personal" || names["k1"] != "Familia" {
		t.Errorf("spaces of the assets = %v", names)
	}
	spaces, err := m.st.Spaces(ctx, "user-1")
	if err != nil || len(spaces) != 2 {
		t.Fatalf("spaces = %+v %v", spaces, err)
	}
}
