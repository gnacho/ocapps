package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/crypto/bcrypt"

	commonauth "github.com/gnacho/ocapps/internal/common/auth"
	"github.com/gnacho/ocapps/internal/news/i18n"
	"github.com/gnacho/ocapps/internal/news/store"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// stubValidator is a Validator with a fixed answer that records the
// credential it saw.
type stubValidator struct {
	user   *store.User
	ok     bool
	got    Credential
	called bool
}

func (s *stubValidator) Validate(_ context.Context, cred Credential) (*store.User, bool) {
	s.called = true
	s.got = cred
	if !s.ok {
		return nil, false
	}
	return s.user, true
}

// basicHeader builds a valid Basic Authorization header.
func basicHeader(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func TestMiddlewareRejectsRequestsWithoutCredentials(t *testing.T) {
	v := &stubValidator{ok: true}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("the wrapped handler must not run without credentials")
	})

	req := httptest.NewRequest(http.MethodGet, "/items", nil)
	w := httptest.NewRecorder()
	Middleware(v, next).ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != `Basic realm="ocnews"` {
		t.Fatalf("WWW-Authenticate = %q", got)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var body errBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Error.Code != "unauthorized" || body.Error.Message != i18n.T(i18n.EN, "unauthorized") {
		t.Fatalf("error body = %+v, want the English unauthorized message", body.Error)
	}
	if v.called {
		t.Fatal("the validator must not be called without credentials")
	}
}

func TestMiddlewareNegotiatesTheErrorMessage(t *testing.T) {
	v := &stubValidator{}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

	req := httptest.NewRequest(http.MethodGet, "/items", nil)
	req.Header.Set("Accept-Language", "es-ES,es;q=0.9,en;q=0.5")
	w := httptest.NewRecorder()
	Middleware(v, next).ServeHTTP(w, req)

	var body errBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if want := i18n.T(i18n.ES, "unauthorized"); body.Error.Message != want {
		t.Fatalf("message = %q, want %q", body.Error.Message, want)
	}
}

func TestMiddlewareRejectsAnInvalidCredential(t *testing.T) {
	v := &stubValidator{ok: false}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("the wrapped handler must not run with an invalid credential")
	})

	req := httptest.NewRequest(http.MethodGet, "/items", nil)
	req.Header.Set("Authorization", basicHeader("alice", "wrong"))
	w := httptest.NewRecorder()
	Middleware(v, next).ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if !v.called {
		t.Fatal("the validator must see the parsed credential")
	}
	if v.got.Username != "alice" || v.got.Password != "wrong" {
		t.Fatalf("credential = %+v", v.got)
	}
}

func TestMiddlewareExposesTheUserAndTheLanguage(t *testing.T) {
	user := &store.User{ID: 7, Username: "alice", Language: "es"}
	v := &stubValidator{user: user, ok: true}

	var gotUser *store.User
	var gotLang i18n.Lang
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser = User(r)
		gotLang = Lang(r)
		w.WriteHeader(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodGet, "/items", nil)
	req.Header.Set("Authorization", basicHeader("alice", "app-token"))
	// The explicit user preference wins over the header.
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	w := httptest.NewRecorder()
	Middleware(v, next).ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want the handler answer", w.Code)
	}
	if gotUser == nil || gotUser.ID != 7 {
		t.Fatalf("user in context = %+v", gotUser)
	}
	if gotLang != i18n.ES {
		t.Fatalf("lang = %q, want the user preference", gotLang)
	}
}

func TestMiddlewareNegotiatesWhenTheUserHasNoPreference(t *testing.T) {
	v := &stubValidator{user: &store.User{ID: 1, Username: "bob", Language: "auto"}, ok: true}
	var gotLang i18n.Lang
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { gotLang = Lang(r) })

	req := httptest.NewRequest(http.MethodGet, "/items", nil)
	req.Header.Set("Authorization", basicHeader("bob", "t"))
	req.Header.Set("Accept-Language", "es-419")
	Middleware(v, next).ServeHTTP(httptest.NewRecorder(), req)

	if gotLang != i18n.ES {
		t.Fatalf("lang = %q, want the header language", gotLang)
	}
}

func TestUserAndLangWithoutContext(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/items", nil)
	if u := User(req); u != nil {
		t.Fatalf("User = %+v, want nil", u)
	}
	if l := Lang(req); l != i18n.EN {
		t.Fatalf("Lang = %q, want the English default", l)
	}
}

func TestCredentialsParsing(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   Credential
		ok     bool
	}{
		{"basic", basicHeader("alice", "app-token"), Credential{Username: "alice", Password: "app-token"}, true},
		{"basic with colon in the password", basicHeader("alice", "a:b"), Credential{Username: "alice", Password: "a:b"}, true},
		{"bearer", "Bearer oidc-token", Credential{Bearer: "oidc-token"}, true},
		{"bearer with spaces", "Bearer   oidc-token  ", Credential{Bearer: "oidc-token"}, true},
		{"no header", "", Credential{}, false},
		{"unknown scheme", "Digest abc", Credential{}, false},
		{"broken base64", "Basic !!!", Credential{}, false},
		{"basic without colon", "Basic " + base64.StdEncoding.EncodeToString([]byte("alice")), Credential{}, false},
		{"basic without user", "Basic " + base64.StdEncoding.EncodeToString([]byte(":pw")), Credential{}, false},
		{"empty bearer", "Bearer    ", Credential{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			got, ok := credentials(req)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("credentials = %+v, %v, want %+v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestHashPasswordVerifies(t *testing.T) {
	hash, err := HashPassword("s3cret")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == "s3cret" {
		t.Fatal("the hash must not be the password")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("s3cret")); err != nil {
		t.Fatalf("the hash does not verify: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("other")); err == nil {
		t.Fatal("another password must not verify")
	}
}

// localUser creates a user with a known bcrypt password.
func localUser(t *testing.T, st *store.Store, username, password string) *store.User {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	id, err := st.CreateUser(username, hash, "Local User", "admin")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	u, err := st.GetUserByID(id)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	return u
}

func TestLocalValidator(t *testing.T) {
	st := newStore(t)
	localUser(t, st, "alice", "s3cret")
	lv := &LocalValidator{Store: st}
	ctx := context.Background()

	u, ok := lv.Validate(ctx, Credential{Username: "alice", Password: "s3cret"})
	if !ok || u == nil || u.Username != "alice" {
		t.Fatalf("valid local credential rejected: %+v %v", u, ok)
	}
	// TouchLogin writes after the row was read, so the store is the source.
	if fresh, err := st.GetUserByID(u.ID); err != nil || fresh.LastLoginAt == 0 {
		t.Fatalf("a successful local login must touch the last login (err=%v)", err)
	}

	if _, ok := lv.Validate(ctx, Credential{Username: "alice", Password: "wrong"}); ok {
		t.Fatal("a wrong password must fail")
	}
	if _, ok := lv.Validate(ctx, Credential{Username: "ghost", Password: "s3cret"}); ok {
		t.Fatal("an unknown user must fail")
	}
	if _, ok := lv.Validate(ctx, Credential{Bearer: "token"}); ok {
		t.Fatal("the local validator is Basic only")
	}
	if _, ok := lv.Validate(ctx, Credential{Password: "s3cret"}); ok {
		t.Fatal("a credential without username must fail")
	}
}

func TestShadowValidateRejectsEmptyCredentials(t *testing.T) {
	st := newStore(t)
	v := newShadowValidator(st, "http://127.0.0.1:1", discardLogger())
	if _, ok := v.Validate(context.Background(), Credential{}); ok {
		t.Fatal("an empty credential must fail without asking the graph")
	}
}

func TestUpsertShadowRefreshesTheDisplayName(t *testing.T) {
	st := newStore(t)
	id, err := st.CreateUserWithOCID("alice", "uuid-alice", "shadow:x", "Old name", "admin")
	if err != nil {
		t.Fatalf("CreateUserWithOCID: %v", err)
	}
	v := NewShadowValidator(nil, st, discardLogger())

	u, err := v.upsertShadow(&commonauth.User{ID: "uuid-alice", Username: "alice", DisplayName: "New name"})
	if err != nil {
		t.Fatalf("upsertShadow: %v", err)
	}
	if u.ID != id {
		t.Fatalf("shadow id = %d, want the existing %d", u.ID, id)
	}
	if u.DisplayName != "New name" {
		t.Fatalf("display name = %q, want the graph one", u.DisplayName)
	}
}

func TestUpsertShadowLinksAPreviousShadowWithoutOCID(t *testing.T) {
	st := newStore(t)
	id, err := st.CreateUser("legacy", "shadow:x", "Legacy", "user")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	v := NewShadowValidator(nil, st, discardLogger())

	u, err := v.upsertShadow(&commonauth.User{ID: "uuid-legacy", Username: "legacy", DisplayName: "Legacy"})
	if err != nil {
		t.Fatalf("upsertShadow: %v", err)
	}
	if u.ID != id {
		t.Fatalf("shadow id = %d, want the linked row %d", u.ID, id)
	}
	if u.OCID != "uuid-legacy" {
		t.Fatalf("ocid = %q, want it linked to the graph identity", u.OCID)
	}
}

func TestUpsertShadowCreatesTheFirstUserAsAdmin(t *testing.T) {
	st := newStore(t)
	v := NewShadowValidator(nil, st, discardLogger())

	u, err := v.upsertShadow(&commonauth.User{ID: "", Username: "first", DisplayName: "First"})
	if err != nil {
		t.Fatalf("upsertShadow: %v", err)
	}
	if u.Role != "admin" {
		t.Fatalf("role = %q, the first user must be admin", u.Role)
	}
	if u.OCID != "" {
		t.Fatalf("ocid = %q, want none for an identity without id", u.OCID)
	}
}

func TestUpsertShadowCreatesTheRestAsUsers(t *testing.T) {
	st := newStore(t)
	v := NewShadowValidator(nil, st, discardLogger())

	if _, err := v.upsertShadow(&commonauth.User{ID: "uuid-1", Username: "first"}); err != nil {
		t.Fatalf("first upsertShadow: %v", err)
	}
	u, err := v.upsertShadow(&commonauth.User{ID: "uuid-2", Username: "second"})
	if err != nil {
		t.Fatalf("second upsertShadow: %v", err)
	}
	if u.Role != "user" {
		t.Fatalf("role = %q, want user after the first one", u.Role)
	}
	if n, _ := st.CountUsers(); n != 2 {
		t.Fatalf("users = %d, want 2", n)
	}
}

// TestShadowValidateCachesAValidCredential checks that the local positive
// cache answers without asking the graph again.
func TestShadowValidateCachesAValidCredential(t *testing.T) {
	st := newStore(t)
	fg := &fakeGraph{password: "app-token", bearer: "bearer-token"}
	ts := httptest.NewServer(fg.handler(t))
	t.Cleanup(ts.Close)
	v := newShadowValidator(st, ts.URL, discardLogger())

	cred := Credential{Username: "nacho", Password: "app-token"}
	first, ok := v.Validate(context.Background(), cred)
	if !ok {
		t.Fatal("valid credential rejected")
	}
	hits := fg.meHits
	second, ok := v.Validate(context.Background(), cred)
	if !ok || second.ID != first.ID {
		t.Fatalf("cached validation = %+v, %v", second, ok)
	}
	if fg.meHits != hits {
		t.Fatalf("graph hits = %d, want %d (the local cache must answer)", fg.meHits, hits)
	}
}

func TestNewShadowValidatorFallsBackToTheDefaultLogger(t *testing.T) {
	v := NewShadowValidator(nil, newStore(t), nil)
	if v.log == nil {
		t.Fatal("a nil logger must fall back to the default one")
	}
}
