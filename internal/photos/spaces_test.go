package photos

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gnacho/ocapps/internal/common/auth"
	"github.com/gnacho/ocapps/internal/common/config"
	"github.com/gnacho/ocapps/internal/photos/store"
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

// TestScanEachSpaceHasItsOwnTimeout: a space that takes longer than the budget
// cannot leave the following spaces unscanned (issue #145). Measured on
// drive.domatix.cloud: the personal library consumed the whole 2h cycle and the
// shares space was never walked.
func TestScanEachSpaceHasItsOwnTimeout(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/graph/v1.0/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "user-1"})
		case r.URL.Path == "/graph/v1.0/me/drives":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{
				{"id": "p1", "name": "nacho", "driveType": "personal",
					"root": map[string]any{"webDavUrl": srv.URL + "/dav/spaces/p"}},
				{"id": "s1", "name": "Shares", "driveType": "virtual",
					"root": map[string]any{"webDavUrl": srv.URL + "/dav/spaces/s"}},
			}})
		case r.Method == "PROPFIND":
			if strings.HasPrefix(r.URL.Path, "/dav/spaces/p") {
				// slow space: it will hit its own deadline
				time.Sleep(400 * time.Millisecond)
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = fmt.Fprint(w, multistatusXML("/dav/spaces/s/", "/dav/spaces/s/foto.jpg"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	m := newSpacesModule(t, srv, config.PhotosConfig{ScanRoot: "/"})
	m.sched.tmo = 150 * time.Millisecond
	ctx := context.Background()
	m.reg.Touch(ctx, &auth.User{ID: "user-1"}, auth.Credential{Bearer: "tok"})

	m.sched.scanOne(ctx, "user-1")

	all, err := m.st.AssetsPage(ctx, "user-1", 1<<62-1, 1<<62-1, 10, false, false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].SpaceName != "Shares" {
		t.Fatalf("the space after the slow one was not scanned: %+v", all)
	}
}

// TestScanPrunesRowsOfLostSpaces: after a complete pass, the rows of a space the
// owner can no longer see are soft-deleted (issue #70). The 31 rows of a drive
// that disappeared from /me/drives stayed alive forever before this.
func TestScanPrunesRowsOfLostSpaces(t *testing.T) {
	srv := fakeDrivesOpenCloud(t)
	m := newSpacesModule(t, srv, config.PhotosConfig{ScanRoot: "Fotos"})
	ctx := context.Background()
	m.reg.Touch(ctx, &auth.User{ID: "user-1"}, auth.Credential{Bearer: "tok"})

	// an asset of a space the listing does not return any more
	if _, _, err := m.st.UpsertInSpace(ctx, "user-1", "/dav/spaces/s$gone/Fotos/vieja.jpg", "e0", "vieja.jpg", "image", time.Now(), 100,
		store.Space{ID: "gone", Name: "Antiguo", Type: "project"}); err != nil {
		t.Fatal(err)
	}

	m.sched.scanOne(ctx, "user-1")

	all, err := m.st.AssetsPage(ctx, "user-1", 1<<62-1, 1<<62-1, 10, false, false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range all {
		if a.SpaceID == "gone" {
			t.Errorf("the lost space was not pruned: %+v", a)
		}
	}
	if len(all) != 2 { // p1 and k1 from the listing
		t.Errorf("live assets = %d, want 2: %+v", len(all), all)
	}
}

// TestScanKeepsRowsOfVisibleSpacesNotSelected: configuring OCAPPS_PHOTOS_SCAN_SPACES
// to a subset of the drives must not delete the photos of the other visible
// spaces (they are simply not walked).
func TestScanKeepsRowsOfVisibleSpacesNotSelected(t *testing.T) {
	srv := fakeDrivesOpenCloud(t)
	m := newSpacesModule(t, srv, config.PhotosConfig{ScanRoot: "Fotos", ScanSpaces: []string{"personal"}})
	ctx := context.Background()
	m.reg.Touch(ctx, &auth.User{ID: "user-1"}, auth.Credential{Bearer: "tok"})

	if _, _, err := m.st.UpsertInSpace(ctx, "user-1", "/dav/spaces/s$k1/Fotos/b.jpg", "e1", "b.jpg", "image", time.Now(), 100,
		store.Space{ID: "k1", Name: "Familia", Type: "project"}); err != nil {
		t.Fatal(err)
	}

	m.sched.scanOne(ctx, "user-1")

	all, err := m.st.AssetsPage(ctx, "user-1", 1<<62-1, 1<<62-1, 10, false, false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range all {
		if a.SpaceID == "k1" {
			found = true
		}
	}
	if !found {
		t.Errorf("a visible space that is not selected was pruned: %+v", all)
	}
}

// TestScanDoesNotPruneAfterAnIncompletePass: if one space fails (a timeout,
// say) the pass cannot conclude that anything is lost, so nothing is pruned.
func TestScanDoesNotPruneAfterAnIncompletePass(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/graph/v1.0/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "user-1"})
		case r.URL.Path == "/graph/v1.0/me/drives":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{
				{"id": "p1", "name": "nacho", "driveType": "personal",
					"root": map[string]any{"webDavUrl": srv.URL + "/dav/spaces/p"}},
				{"id": "s1", "name": "Shares", "driveType": "virtual",
					"root": map[string]any{"webDavUrl": srv.URL + "/dav/spaces/s"}},
			}})
		case r.Method == "PROPFIND":
			if strings.HasPrefix(r.URL.Path, "/dav/spaces/p") {
				time.Sleep(200 * time.Millisecond) // hits its own deadline
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = fmt.Fprint(w, multistatusXML("/dav/spaces/s/", "/dav/spaces/s/foto.jpg"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	m := newSpacesModule(t, srv, config.PhotosConfig{ScanRoot: "Fotos"})
	m.sched.tmo = 100 * time.Millisecond
	ctx := context.Background()
	m.reg.Touch(ctx, &auth.User{ID: "user-1"}, auth.Credential{Bearer: "tok"})

	if _, _, err := m.st.UpsertInSpace(ctx, "user-1", "/dav/spaces/s$gone/Fotos/vieja.jpg", "e0", "vieja.jpg", "image", time.Now(), 100,
		store.Space{ID: "gone", Name: "Antiguo", Type: "project"}); err != nil {
		t.Fatal(err)
	}

	m.sched.scanOne(ctx, "user-1")

	all, _ := m.st.AssetsPage(ctx, "user-1", 1<<62-1, 1<<62-1, 10, false, false, "", "")
	pruned := true
	for _, a := range all {
		if a.SpaceID == "gone" {
			pruned = false
		}
	}
	if pruned {
		t.Errorf("an incomplete pass pruned rows: %+v", all)
	}
}
