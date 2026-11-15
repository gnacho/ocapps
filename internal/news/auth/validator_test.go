package auth

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	commonauth "github.com/gnacho/ocapps/internal/common/auth"
	"github.com/gnacho/ocapps/internal/news/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// newShadowValidator: glue on top of the common Graph validator (MultiTenant, as
// in the module wiring, SPEC §6.2).
func newShadowValidator(st *store.Store, graphURL string, log *slog.Logger) *ShadowValidator {
	gv := commonauth.NewGraphValidator(graphURL, commonauth.MultiTenant(), log)
	return NewShadowValidator(gv, st, log)
}

// fakeGraph: a fake OpenCloud, it validates Basic (fixed user) and Bearer (fixed token),
// both resolve to the SAME /me (same oc_id).
type fakeGraph struct {
	mu       sync.Mutex
	meHits   int
	password string
	bearer   string
}

func (f *fakeGraph) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.meHits++
		f.mu.Unlock()
		if r.URL.Path != "/graph/v1.0/me" {
			http.NotFound(w, r)
			return
		}
		ok := false
		if user, pass, b := r.BasicAuth(); b {
			ok = user == "nacho" && pass == f.password
		} else if h := r.Header.Get("Authorization"); h != "" {
			ok = h == "Bearer "+f.bearer
		}
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"uuid-1234","displayName":"Nacho Real","onPremisesSamAccountName":"nacho"}`)
	})
}

func TestOpenCloudValidatorBasicAndBearerUnify(t *testing.T) {
	st := newStore(t)
	fg := &fakeGraph{password: "app-token-xyz", bearer: "oidc-access-tok"}
	ts := httptest.NewServer(fg.handler(t))
	t.Cleanup(ts.Close)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	v := newShadowValidator(st, ts.URL, log)

	// Bad Basic -> false. NOTE: the common validator caches the 401s for 30 s
	// by key basic:<user> (SPEC §6.1), so the bad attempt uses ANOTHER
	// username so as not to poison the "nacho" entry in this test.
	if u, ok := v.Validate(context.Background(), Credential{Username: "nobody", Password: "badpass"}); ok || u != nil {
		t.Fatal("a bad password must fail")
	}
	// Good Basic -> admin shadow with the Graph displayName
	uBasic, ok := v.Validate(context.Background(), Credential{Username: "nacho", Password: "app-token-xyz"})
	if !ok || uBasic.Username != "nacho" || uBasic.DisplayName != "Nacho Real" || uBasic.OCID != "uuid-1234" {
		t.Fatalf("shadow Basic mal: %+v", uBasic)
	}
	if uBasic.Role != "admin" {
		t.Fatalf("the first user must be admin: %s", uBasic.Role)
	}

	// Bearer (web session) -> SAME user (oc_id), without duplicating
	uBearer, ok := v.Validate(context.Background(), Credential{Bearer: "oidc-access-tok"})
	if !ok {
		t.Fatal("valid bearer rejected")
	}
	if uBearer.ID != uBasic.ID {
		t.Fatalf("Basic and Bearer must resolve to the same shadow: %d vs %d", uBasic.ID, uBearer.ID)
	}

	// Bad Bearer -> false
	if _, ok := v.Validate(context.Background(), Credential{Bearer: "expired"}); ok {
		t.Fatal("a bad bearer must fail")
	}

	// cache: the same credential does not hit the Graph again
	before := fg.meHits
	if _, ok := v.Validate(context.Background(), Credential{Bearer: "oidc-access-tok"}); !ok {
		t.Fatal("bearer revalidation failed")
	}
	if fg.meHits != before {
		t.Fatalf("cache not applied: %d new hits", fg.meHits-before)
	}

	// LocalValidator NEVER accepts Bearer nor the shadow hash
	lv := &LocalValidator{Store: st}
	if _, ok := lv.Validate(context.Background(), Credential{Bearer: "oidc-access-tok"}); ok {
		t.Fatal("local must not accept bearer")
	}
	if _, ok := lv.Validate(context.Background(), Credential{Username: "nacho", Password: "app-token-xyz"}); ok {
		t.Fatal("the shadow must not authenticate locally")
	}
}

// TestOpenCloudValidatorConcurrentShadowCreate: two credentials of the SAME
// OpenCloud user (Basic app-token and Bearer web session) come in at the same time into
// an empty DB. Both must resolve to the same shadow WITHOUT a UNIQUE error
// (read-then-create race in upsertShadow, issue #18).
func TestOpenCloudValidatorConcurrentShadowCreate(t *testing.T) {
	st := newStore(t)
	fg := &fakeGraph{password: "app-token-xyz", bearer: "oidc-access-tok"}
	ts := httptest.NewServer(fg.handler(t))
	t.Cleanup(ts.Close)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	v := newShadowValidator(st, ts.URL, log)

	const n = 32
	creds := make([]Credential, n)
	for i := range creds {
		if i%2 == 0 {
			creds[i] = Credential{Username: "nacho", Password: "app-token-xyz"}
		} else {
			creds[i] = Credential{Bearer: "oidc-access-tok"}
		}
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	ids := make([]int64, n)
	for i := range creds {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			u, ok := v.Validate(context.Background(), creds[i])
			if !ok {
				errs[i] = fmt.Errorf("validate failed with credential %d", i)
				return
			}
			ids[i] = u.ID
		}(i)
	}
	wg.Wait()

	var firstID int64
	for i := range creds {
		if errs[i] != nil {
			t.Fatalf("request %d: %v (UNIQUE race?)", i, errs[i])
		}
		if i == 0 {
			firstID = ids[i]
		} else if ids[i] != firstID {
			t.Fatalf("request %d resolves to another shadow: %d != %d", i, ids[i], firstID)
		}
	}
	// the DB must have a SINGLE shadow user
	if nUsers, err := st.CountUsers(); err != nil || nUsers != 1 {
		t.Fatalf("expected 1 user, there are %d (err=%v)", nUsers, err)
	}
}
