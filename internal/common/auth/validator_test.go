package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// graphStub simulates OpenCloud's /graph/v1.0/me.
type graphStub struct {
	srv      *httptest.Server
	requests atomic.Int32
	delay    time.Duration
}

func newGraphStub(t *testing.T) *graphStub {
	t.Helper()
	g := &graphStub{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.requests.Add(1)
		if g.delay > 0 {
			time.Sleep(g.delay)
		}
		if r.URL.Path != "/graph/v1.0/me" {
			http.NotFound(w, r)
			return
		}
		switch {
		case r.Header.Get("Authorization") == "Bearer good-token":
			g.writeMe(w, "id-alice")
		case strings.HasPrefix(r.Header.Get("Authorization"), "Basic "):
			u, p, ok := r.BasicAuth()
			if !ok || u != "alice" || p != "app-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			g.writeMe(w, "id-alice")
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *graphStub) writeMe(w http.ResponseWriter, id string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"id":                       id,
		"displayName":              "Alice Liddell",
		"onPremisesSamAccountName": "alice",
		"userPrincipalName":        "alice@example.com",
		"mail":                     "alice@example.com",
	})
}

func (g *graphStub) validator(p Policy) *GraphValidator {
	return NewGraphValidator(g.srv.URL, p, slog.Default())
}

// (1) cache hit: within the TTL no request is re-issued to Graph.
func TestCacheHitDoesNotRequestAgain(t *testing.T) {
	g := newGraphStub(t)
	v := g.validator(MultiTenant())

	for i := 0; i < 3; i++ {
		u, ok := v.Validate(context.Background(), Credential{Bearer: "good-token"})
		if !ok || u.ID != "id-alice" {
			t.Fatalf("validation %d: ok=%v u=%+v", i, ok, u)
		}
	}
	if n := g.requests.Load(); n != 1 {
		t.Fatalf("requests to Graph: got %d, want 1 (cache hit not working)", n)
	}
}

// (2) singleflight: N concurrent validations with the same token ->
// a SINGLE request to Graph.
func TestSingleflightOneRequest(t *testing.T) {
	g := newGraphStub(t)
	g.delay = 100 * time.Millisecond // guarantees the N goroutines overlap
	v := g.validator(MultiTenant())

	const n = 16
	var wg sync.WaitGroup
	okCount := atomic.Int32{}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if u, ok := v.Validate(context.Background(), Credential{Bearer: "good-token"}); ok && u.ID == "id-alice" {
				okCount.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := okCount.Load(); got != n {
		t.Fatalf("ok validations: got %d, want %d", got, n)
	}
	if got := g.requests.Load(); got != 1 {
		t.Fatalf("requests to Graph: got %d, want 1 (singleflight broken)", got)
	}
}

// (3) 401 cached for 30 s: a bad token in a loop does not hammer Graph.
func TestNegativeCachedResult(t *testing.T) {
	g := newGraphStub(t)
	v := g.validator(MultiTenant())

	for i := 0; i < 5; i++ {
		if _, ok := v.Validate(context.Background(), Credential{Bearer: "bad-token"}); ok {
			t.Fatalf("bad token validated on attempt %d", i)
		}
	}
	if n := g.requests.Load(); n != 1 {
		t.Fatalf("requests to Graph: got %d, want 1 (401 not cached)", n)
	}
}

// (4) policies: MultiTenant admits everyone, SingleTenant only its oc_id.
func TestPoliticas(t *testing.T) {
	g := newGraphStub(t)
	cred := Credential{Bearer: "good-token"}

	if _, ok := g.validator(MultiTenant()).Validate(context.Background(), cred); !ok {
		t.Fatal("MultiTenant rejected a valid user")
	}
	if _, ok := g.validator(SingleTenant("id-alice")).Validate(context.Background(), cred); !ok {
		t.Fatal("SingleTenant(correct id) rejected the tenant")
	}
	if _, ok := g.validator(SingleTenant("id-other")).Validate(context.Background(), cred); ok {
		t.Fatal("SingleTenant(different id) admitted another user")
	}
	if SingleTenant("").Admit(&User{ID: ""}) {
		t.Fatal("SingleTenant(\"\") must never admit")
	}
}

// (5) Basic and Bearer resolve to the SAME user by oc_id.
func TestBasicBearerSameUser(t *testing.T) {
	g := newGraphStub(t)
	v := g.validator(MultiTenant())

	ub, ok := v.Validate(context.Background(), Credential{Username: "alice", Password: "app-token"})
	if !ok {
		t.Fatal("basic rejected")
	}
	ut, ok := v.Validate(context.Background(), Credential{Bearer: "good-token"})
	if !ok {
		t.Fatal("bearer rejected")
	}
	if ub.ID != ut.ID || ub.ID != "id-alice" {
		t.Fatalf("different oc_id: basic=%q bearer=%q", ub.ID, ut.ID)
	}
	if ub.Username != "alice" || ut.Username != "alice" {
		t.Fatalf("username: basic=%q bearer=%q", ub.Username, ut.Username)
	}
	if ub.Email != "alice@example.com" || ub.DisplayName != "Alice Liddell" {
		t.Fatalf("graphMe mapping: %+v", ub)
	}
}

// The Basic cache is indexed by user AND password (hash, SPEC §6.1): a
// different password for the same user does NOT come from the cache - it goes
// to Graph and fails (B1: with the old key `basic:<user>` any password got in
// for 5 min after a legitimate login).
func TestBasicCachePerUser(t *testing.T) {
	g := newGraphStub(t)
	v := g.validator(MultiTenant())

	if _, ok := v.Validate(context.Background(), Credential{Username: "alice", Password: "app-token"}); !ok {
		t.Fatal("basic rejected")
	}
	if _, ok := v.Validate(context.Background(), Credential{Username: "alice", Password: "other"}); ok {
		t.Fatal("different password admitted from cache (B1)")
	}
	// 2 requests: the good one (200) and the bad one (401), the second was NOT a cache hit.
	if n := g.requests.Load(); n != 2 {
		t.Fatalf("requests to Graph: got %d, want 2 (the different password must not come from cache)", n)
	}
	// and the good one still comes from the cache with no new request.
	if _, ok := v.Validate(context.Background(), Credential{Username: "alice", Password: "app-token"}); !ok {
		t.Fatal("the good credential should still come from the cache")
	}
	if n := g.requests.Load(); n != 2 {
		t.Fatalf("requests to Graph: got %d, want 2", n)
	}
}

// DoS through a negative cache (B1): an attempt with a BAD password cannot
// invalidate the cached entry of the good password of the same user - each
// password has its own key.
func TestPasswordMalaNoInvalidaLaBuena(t *testing.T) {
	g := newGraphStub(t)
	v := g.validator(MultiTenant())

	good := Credential{Username: "alice", Password: "app-token"}
	bad := Credential{Username: "alice", Password: "mala"}

	if _, ok := v.Validate(context.Background(), good); !ok {
		t.Fatal("good basic rejected")
	}
	// attacker: bad password in a loop -> 401 cached for 30 s UNDER ITS KEY.
	for i := 0; i < 3; i++ {
		if _, ok := v.Validate(context.Background(), bad); ok {
			t.Fatal("bad password validated")
		}
	}
	// the legitimate user still gets in through the cache, with no new request to Graph.
	before := g.requests.Load()
	if _, ok := v.Validate(context.Background(), good); !ok {
		t.Fatal("DoS: the bad password invalidated the cached entry of the good one")
	}
	if n := g.requests.Load(); n != before {
		t.Fatalf("extra requests to Graph: got %d, want %d", n, before)
	}
	// and the bad loop only cost ONE request (negative cached under its key).
	if n := g.requests.Load(); n != 2 {
		t.Fatalf("requests to Graph: got %d, want 2 (1 good + 1 bad cached)", n)
	}
}

// M7: a network failure or a 5xx from Graph is NOT cached as negative - every
// attempt retries against Graph and a recovered Graph validates again
// immediately (without waiting for the 30 s of the negative cache).
func TestTransientFailuresAreNotCached(t *testing.T) {
	t.Run("5xx not cached", func(t *testing.T) {
		var requests atomic.Int32
		var fail atomic.Bool
		fail.Store(true)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			if fail.Load() {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "id-alice"})
		}))
		t.Cleanup(srv.Close)
		v := NewGraphValidator(srv.URL, MultiTenant(), slog.Default())
		cred := Credential{Bearer: "tok"}

		for i := 0; i < 3; i++ {
			if _, ok := v.Validate(context.Background(), cred); ok {
				t.Fatal("validated with Graph 502")
			}
		}
		if n := requests.Load(); n != 3 {
			t.Fatalf("requests to Graph: got %d, want 3 (the 5xx must not be cached as negative)", n)
		}
		// Graph recovers: the next validation gets in without waiting 30 s.
		fail.Store(false)
		if _, ok := v.Validate(context.Background(), cred); !ok {
			t.Fatal("Graph recovered but validation still fails (negative cached improperly)")
		}
	})

	t.Run("network down not cached", func(t *testing.T) {
		v := NewGraphValidator("http://127.0.0.1:1", MultiTenant(), slog.Default())
		for i := 0; i < 2; i++ {
			if _, ok := v.Validate(context.Background(), Credential{Bearer: "x"}); ok {
				t.Fatal("validated with Graph unreachable")
			}
		}
		// no negative cache: the key does not exist in the cache.
		if _, _, hit := v.lookup(cacheKey(Credential{Bearer: "x"})); hit {
			t.Fatal("network failure cached as negative (M7)")
		}
	})
}

func TestURLNormalizacion(t *testing.T) {
	for in, want := range map[string]string{
		"https://cloud.example.com":                "https://cloud.example.com/graph/v1.0/me",
		"https://cloud.example.com/":               "https://cloud.example.com/graph/v1.0/me",
		"https://cloud.example.com/graph/v1.0/me":  "https://cloud.example.com/graph/v1.0/me",
		"https://cloud.example.com/graph/v1.0/me/": "https://cloud.example.com/graph/v1.0/me",
	} {
		if got := NewGraphValidator(in, nil, nil).meURL; got != want {
			t.Errorf("%s: got %q, want %q", in, got, want)
		}
	}
}

func TestEmptyCredential(t *testing.T) {
	g := newGraphStub(t)
	v := g.validator(MultiTenant())
	if _, ok := v.Validate(context.Background(), Credential{}); ok {
		t.Fatal("empty credential validated")
	}
	if n := g.requests.Load(); n != 0 {
		t.Fatalf("Graph called with an empty credential: %d requests", n)
	}
}

func TestGraphInalcanzable(t *testing.T) {
	v := NewGraphValidator("http://127.0.0.1:1", MultiTenant(), slog.Default())
	if _, ok := v.Validate(context.Background(), Credential{Bearer: "x"}); ok {
		t.Fatal("validated with Graph unreachable")
	}
}

// Middleware: 401 with WWW-Authenticate without credentials, it passes the
// *User to the context with valid credentials.
func TestMiddleware(t *testing.T) {
	g := newGraphStub(t)
	v := g.validator(MultiTenant())

	var gotUser *User
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser = UserFrom(r)
		w.WriteHeader(http.StatusOK)
	})
	h := Middleware(v, next)

	t.Run("no credentials -> 401 + WWW-Authenticate", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		res := rec.Result()
		defer res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("got %d", res.StatusCode)
		}
		if wa := res.Header.Get("WWW-Authenticate"); !strings.HasPrefix(wa, "Basic ") {
			t.Fatalf("WWW-Authenticate: %q", wa)
		}
	})

	t.Run("valid basic -> 200 and user in context", func(t *testing.T) {
		gotUser = nil
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.SetBasicAuth("alice", "app-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Result().StatusCode != http.StatusOK {
			t.Fatalf("got %d", rec.Result().StatusCode)
		}
		if gotUser == nil || gotUser.ID != "id-alice" || gotUser.Username != "alice" {
			t.Fatalf("user in ctx: %+v", gotUser)
		}
	})

	t.Run("valid bearer -> 200", func(t *testing.T) {
		gotUser = nil
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.Header.Set("Authorization", "Bearer good-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Result().StatusCode != http.StatusOK || gotUser == nil {
			t.Fatalf("code=%d user=%+v", rec.Result().StatusCode, gotUser)
		}
	})

	t.Run("bad bearer -> 401", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.Header.Set("Authorization", "Bearer bad-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Result().StatusCode != http.StatusUnauthorized {
			t.Fatalf("got %d", rec.Result().StatusCode)
		}
	})

	t.Run("malformed basic -> 401", func(t *testing.T) {
		for _, hval := range []string{"Basic !!!", "Basic " + "c2luLWRvcy1wdW50b3M=", "Bearer "} {
			r := httptest.NewRequest(http.MethodGet, "/x", nil)
			r.Header.Set("Authorization", hval)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Result().StatusCode != http.StatusUnauthorized {
				t.Fatalf("%q: got %d", hval, rec.Result().StatusCode)
			}
		}
	})
}

// WithPolicy (H5, SPEC §6.2): the derived validator shares the cache with the
// base one (a single request to Graph across modules) but applies its own
// policy - SingleTenant in photos does not inherit the MultiTenant admission.
func TestWithPolicySharesCacheNotPolicy(t *testing.T) {
	g := newGraphStub(t)
	base := g.validator(MultiTenant())
	photos := base.WithPolicy(SingleTenant("id-other"))

	// photos validates first: valid user but NOT the tenant -> rejected.
	if _, ok := photos.Validate(context.Background(), Credential{Bearer: "good-token"}); ok {
		t.Fatal("WithPolicy(SingleTenant other) admitted the user")
	}
	// the admission stayed cached: base (MultiTenant) gets in WITHOUT a new request.
	before := g.requests.Load()
	if _, ok := base.Validate(context.Background(), Credential{Bearer: "good-token"}); !ok {
		t.Fatal("MultiTenant base rejected with the cache already warm")
	}
	if n := g.requests.Load(); n != before || n != 1 {
		t.Fatalf("requests to Graph: got %d, want 1 (cache not shared)", n)
	}
	// and the correct single-tenant validator does admit the tenant.
	if _, ok := base.WithPolicy(SingleTenant("id-alice")).Validate(context.Background(),
		Credential{Bearer: "good-token"}); !ok {
		t.Fatal("WithPolicy(SingleTenant correct) rejected the tenant")
	}
}

func ExampleCredential() {
	c := Credential{Bearer: "session-token"}
	fmt.Println(c.Bearer != "")
	// Output: true
}
