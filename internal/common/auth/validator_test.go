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

// graphStub simula el /graph/v1.0/me de OpenCloud.
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

// (1) cache hit: dentro del TTL no se re-emite petición a Graph.
func TestCacheHitNoReemite(t *testing.T) {
	g := newGraphStub(t)
	v := g.validator(MultiTenant())

	for i := 0; i < 3; i++ {
		u, ok := v.Validate(context.Background(), Credential{Bearer: "good-token"})
		if !ok || u.ID != "id-alice" {
			t.Fatalf("validación %d: ok=%v u=%+v", i, ok, u)
		}
	}
	if n := g.requests.Load(); n != 1 {
		t.Fatalf("peticiones a Graph: got %d, want 1 (cache hit no funciona)", n)
	}
}

// (2) singleflight: N validaciones concurrentes con el mismo token →
// UNA sola petición a Graph.
func TestSingleflightUnaPeticion(t *testing.T) {
	g := newGraphStub(t)
	g.delay = 100 * time.Millisecond // garantiza solape de las N goroutines
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
		t.Fatalf("validaciones ok: got %d, want %d", got, n)
	}
	if got := g.requests.Load(); got != 1 {
		t.Fatalf("peticiones a Graph: got %d, want 1 (singleflight roto)", got)
	}
}

// (3) 401 cacheado 30 s: un token malo en bucle no martillea Graph.
func TestNegativaCacheada(t *testing.T) {
	g := newGraphStub(t)
	v := g.validator(MultiTenant())

	for i := 0; i < 5; i++ {
		if _, ok := v.Validate(context.Background(), Credential{Bearer: "bad-token"}); ok {
			t.Fatalf("token malo validado en intento %d", i)
		}
	}
	if n := g.requests.Load(); n != 1 {
		t.Fatalf("peticiones a Graph: got %d, want 1 (401 no cacheado)", n)
	}
}

// (4) políticas: MultiTenant admite a todos; SingleTenant solo a su oc_id.
func TestPoliticas(t *testing.T) {
	g := newGraphStub(t)
	cred := Credential{Bearer: "good-token"}

	if _, ok := g.validator(MultiTenant()).Validate(context.Background(), cred); !ok {
		t.Fatal("MultiTenant rechazó un usuario válido")
	}
	if _, ok := g.validator(SingleTenant("id-alice")).Validate(context.Background(), cred); !ok {
		t.Fatal("SingleTenant(id correcto) rechazó al tenant")
	}
	if _, ok := g.validator(SingleTenant("id-otro")).Validate(context.Background(), cred); ok {
		t.Fatal("SingleTenant(id distinto) admitió a otro usuario")
	}
	if SingleTenant("").Admit(&User{ID: ""}) {
		t.Fatal("SingleTenant(\"\") no debe admitir nunca")
	}
}

// (5) Basic y Bearer resuelven al MISMO usuario por oc_id.
func TestBasicBearerMismoUsuario(t *testing.T) {
	g := newGraphStub(t)
	v := g.validator(MultiTenant())

	ub, ok := v.Validate(context.Background(), Credential{Username: "alice", Password: "app-token"})
	if !ok {
		t.Fatal("basic rechazado")
	}
	ut, ok := v.Validate(context.Background(), Credential{Bearer: "good-token"})
	if !ok {
		t.Fatal("bearer rechazado")
	}
	if ub.ID != ut.ID || ub.ID != "id-alice" {
		t.Fatalf("oc_id distinto: basic=%q bearer=%q", ub.ID, ut.ID)
	}
	if ub.Username != "alice" || ut.Username != "alice" {
		t.Fatalf("username: basic=%q bearer=%q", ub.Username, ut.Username)
	}
	if ub.Email != "alice@example.com" || ub.DisplayName != "Alice Liddell" {
		t.Fatalf("mapeo graphMe: %+v", ub)
	}
}

// La caché Basic se indexa por usuario (SPEC §6.1): segunda validación con
// otra contraseña dentro del TTL sigue entrando por caché (ventana de
// revocación documentada, ≤5 min).
func TestCacheBasicPorUsuario(t *testing.T) {
	g := newGraphStub(t)
	v := g.validator(MultiTenant())

	if _, ok := v.Validate(context.Background(), Credential{Username: "alice", Password: "app-token"}); !ok {
		t.Fatal("basic rechazado")
	}
	if _, ok := v.Validate(context.Background(), Credential{Username: "alice", Password: "otra"}); !ok {
		t.Fatal("segunda validación dentro del TTL debería salir de caché")
	}
	if n := g.requests.Load(); n != 1 {
		t.Fatalf("peticiones a Graph: got %d, want 1", n)
	}
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

func TestCredencialVacia(t *testing.T) {
	g := newGraphStub(t)
	v := g.validator(MultiTenant())
	if _, ok := v.Validate(context.Background(), Credential{}); ok {
		t.Fatal("credencial vacía validada")
	}
	if n := g.requests.Load(); n != 0 {
		t.Fatalf("Graph llamado con credencial vacía: %d peticiones", n)
	}
}

func TestGraphInalcanzable(t *testing.T) {
	v := NewGraphValidator("http://127.0.0.1:1", MultiTenant(), slog.Default())
	if _, ok := v.Validate(context.Background(), Credential{Bearer: "x"}); ok {
		t.Fatal("validó con Graph inalcanzable")
	}
}

// Middleware: 401 con WWW-Authenticate sin credenciales; pasa el *User al
// contexto con credenciales válidas.
func TestMiddleware(t *testing.T) {
	g := newGraphStub(t)
	v := g.validator(MultiTenant())

	var gotUser *User
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser = UserFrom(r)
		w.WriteHeader(http.StatusOK)
	})
	h := Middleware(v, next)

	t.Run("sin credenciales → 401 + WWW-Authenticate", func(t *testing.T) {
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

	t.Run("basic válido → 200 y user en contexto", func(t *testing.T) {
		gotUser = nil
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.SetBasicAuth("alice", "app-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Result().StatusCode != http.StatusOK {
			t.Fatalf("got %d", rec.Result().StatusCode)
		}
		if gotUser == nil || gotUser.ID != "id-alice" || gotUser.Username != "alice" {
			t.Fatalf("user en ctx: %+v", gotUser)
		}
	})

	t.Run("bearer válido → 200", func(t *testing.T) {
		gotUser = nil
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.Header.Set("Authorization", "Bearer good-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Result().StatusCode != http.StatusOK || gotUser == nil {
			t.Fatalf("code=%d user=%+v", rec.Result().StatusCode, gotUser)
		}
	})

	t.Run("bearer malo → 401", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.Header.Set("Authorization", "Bearer bad-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Result().StatusCode != http.StatusUnauthorized {
			t.Fatalf("got %d", rec.Result().StatusCode)
		}
	})

	t.Run("basic malformado → 401", func(t *testing.T) {
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

// WithPolicy (H5, SPEC §6.2): el validador derivado comparte la caché con el
// base (una sola petición a Graph entre módulos) pero aplica su propia
// política — SingleTenant en photos no hereda la admisión MultiTenant.
func TestWithPolicyComparteCacheNoPolitica(t *testing.T) {
	g := newGraphStub(t)
	base := g.validator(MultiTenant())
	photos := base.WithPolicy(SingleTenant("id-otro"))

	// photos valida primero: usuario válido pero NO es el tenant → rechazado.
	if _, ok := photos.Validate(context.Background(), Credential{Bearer: "good-token"}); ok {
		t.Fatal("WithPolicy(SingleTenant otro) admitió al usuario")
	}
	// la admisión quedó cacheada: base (MultiTenant) entra SIN nueva petición.
	before := g.requests.Load()
	if _, ok := base.Validate(context.Background(), Credential{Bearer: "good-token"}); !ok {
		t.Fatal("base MultiTenant rechazó con la caché ya caliente")
	}
	if n := g.requests.Load(); n != before || n != 1 {
		t.Fatalf("peticiones a Graph: got %d, want 1 (caché no compartida)", n)
	}
	// y el validador single-tenant correcto sí admite al tenant.
	if _, ok := base.WithPolicy(SingleTenant("id-alice")).Validate(context.Background(),
		Credential{Bearer: "good-token"}); !ok {
		t.Fatal("WithPolicy(SingleTenant correcto) rechazó al tenant")
	}
}

func ExampleCredential() {
	c := Credential{Bearer: "token-de-sesion"}
	fmt.Println(c.Bearer != "")
	// Output: true
}
