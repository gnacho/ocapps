package photos

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gnacho/ocapps/internal/common/auth"
	"github.com/gnacho/ocapps/internal/common/config"
)

// TestRegistryConcurrencyTouchDavFor: Touch (writer) against DavFor/get/
// SpacePrefix/scanOne (readers) - the race of the H8 review (sess.dav read
// outside the mutex). It must run with -race.
func TestRegistryConcurrencyTouchDavFor(t *testing.T) {
	srv := fakeOpenCloud(t, propfindOnePhoto)
	m, err := New(testConfig(config.Common{OpenCloudURL: srv.URL, PhotosDataDir: t.TempDir()},
		config.PhotosConfig{ScanEvery: time.Hour}), slog.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(2)
		go func(w int) { // writers: Touch with changing tokens
			defer wg.Done()
			for i := 0; i < 200; i++ {
				m.reg.Touch(ctx, &auth.User{ID: "user-1"}, auth.Credential{Bearer: fmt.Sprintf("tok-%d-%d", w, i)})
			}
		}(w)
		go func() { // readers: DavFor + get + SpacePrefix + resolveSpace (like scanOne)
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_ = m.reg.DavFor(ctx, "user-1")
				sess := m.reg.get("user-1")
				if sess != nil {
					_, _ = m.reg.resolveSpace(ctx, sess, "user-1")
				}
			}
		}()
	}
	wg.Wait()
}

// TestTouchDoesNotDegradeSeededSession: a user of OCAPPS_PHOTOS_USERS who opens
// the web (Touch with an ephemeral OIDC Bearer) does NOT lose its app token: the
// background scan of the ticker keeps using Basic after the Bearer expires (H8
// review FIX 2).
func TestTouchDoesNotDegradeSeededSession(t *testing.T) {
	// OpenCloud whose PROPFIND ONLY accepts the Basic app token (the expired
	// Bearer receives a 401): if the scan used the Bearer it would fail.
	srv := fakeOpenCloudReq(t, func(r *http.Request) (int, string) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "admin" || p != "app-token" {
			return http.StatusUnauthorized, ""
		}
		return propfindOnePhoto(r.URL.Path)
	})
	m, err := New(testConfig(config.Common{OpenCloudURL: srv.URL, PhotosDataDir: t.TempDir()},
		config.PhotosConfig{ScanRoot: "Fotos", ScanEvery: time.Hour}), slog.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner, err := m.reg.seed(ctx, "admin", "app-token")
	if err != nil {
		t.Fatal(err)
	}
	davSembrado := m.reg.DavFor(ctx, owner)
	if davSembrado == nil {
		t.Fatal("no dav after seeding")
	}
	// the user opens the web: Touch with its OIDC Bearer
	m.reg.Touch(ctx, &auth.User{ID: owner}, auth.Credential{Bearer: "oidc-efimero"})

	// the session keeps the Basic client with the app token
	sess := m.reg.get(owner)
	if sess == nil || !sess.isBasic || !sess.seeded {
		t.Fatalf("session after the Bearer Touch: %+v, it must stay seeded and Basic", sess)
	}
	if got := m.reg.DavFor(ctx, owner); got != davSembrado {
		t.Fatal("Touch replaced the seeded dav with the ephemeral Bearer")
	}
	// and the scan (ticker) keeps indexing with the app token
	m.sched.scanOne(ctx, owner)
	stats, err := m.st.Stats(ctx, owner)
	if err != nil || stats["assets"].(int64) != 1 {
		t.Fatalf("scan with the app token after the Bearer Touch: %v %v", stats, err)
	}
}

// TestDavForHidesExpiredSessions: DavFor returns nil for a Bearer session marked
// as expired (401/403 detected by a scan) until the next Touch of the user
// refreshes it with the new token (H8 review).
func TestDavForHidesExpiredSessions(t *testing.T) {
	m, err := New(testConfig(config.Common{OpenCloudURL: "http://127.0.0.1:1", PhotosDataDir: t.TempDir()},
		config.PhotosConfig{ScanEvery: time.Hour}), slog.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m.reg.Touch(ctx, &auth.User{ID: "user-1"}, auth.Credential{Bearer: "tok-1"})
	if m.reg.DavFor(ctx, "user-1") == nil {
		t.Fatal("dav expected after Touch")
	}
	m.reg.markExpired("user-1")
	if m.reg.DavFor(ctx, "user-1") != nil {
		t.Fatal("an expired session must not serve its dav")
	}
	// the next user request (Touch with a new token) recovers it
	m.reg.Touch(ctx, &auth.User{ID: "user-1"}, auth.Credential{Bearer: "tok-2"})
	if m.reg.DavFor(ctx, "user-1") == nil {
		t.Fatal("Touch with a fresh token must recover the session")
	}
}

// The scan can be limited to a specific drive (issue #39): the personal one is no
// good when the photos live in another drive of the same owner.
func TestResolveSpaceConfigured(t *testing.T) {
	srv := fakeOpenCloud(t, propfindOnePhoto)
	m, err := New(testConfig(config.Common{OpenCloudURL: srv.URL, PhotosDataDir: t.TempDir()},
		config.PhotosConfig{ScanEvery: time.Hour, ScanSpace: "d1"}), slog.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m.reg.Touch(ctx, &auth.User{ID: "user-1"}, auth.Credential{Bearer: "tok"})
	sess := m.reg.get("user-1")

	got, err := m.reg.resolveSpace(ctx, sess, "user-1")
	if err != nil {
		t.Fatalf("resolveSpace: %v", err)
	}
	if !strings.Contains(got, "/dav/spaces/1") {
		t.Errorf("space: got %q", got)
	}

	// An id that is not among the visible drives is a clear error, not a silent
	// fallback to the personal one.
	m.reg.spaces = []string{"no-existe"}
	if _, err := m.reg.resolveSpace(ctx, sess, "user-1"); err == nil {
		t.Error("expected an error with a non-existent space")
	}
}
