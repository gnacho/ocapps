package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusTeapot, map[string]string{"a": "b"})
	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusTeapot {
		t.Fatalf("código: got %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type: got %q", ct)
	}
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil || body["a"] != "b" {
		t.Fatalf("body: %v %v", body, err)
	}
}

func TestErrorStatus(t *testing.T) {
	t.Run("HTTPError", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		ErrorStatus(rec, r, NewError(http.StatusConflict, "conflict", "ya existe"))
		res := rec.Result()
		defer res.Body.Close()
		if res.StatusCode != http.StatusConflict {
			t.Fatalf("código: got %d", res.StatusCode)
		}
		var b struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.NewDecoder(res.Body).Decode(&b); err != nil {
			t.Fatal(err)
		}
		if b.Error.Code != "conflict" || b.Error.Message != "ya existe" {
			t.Fatalf("envelope: %+v", b.Error)
		}
	})
	t.Run("error genérico → 500 sin filtrar mensaje", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		ErrorStatus(rec, r, fmt.Errorf("detalle interno: %w", errors.New("boom")))
		res := rec.Result()
		defer res.Body.Close()
		if res.StatusCode != http.StatusInternalServerError {
			t.Fatalf("código: got %d", res.StatusCode)
		}
		body, _ := io.ReadAll(res.Body)
		if strings.Contains(string(body), "boom") {
			t.Fatalf("filtra detalle interno al cliente: %s", body)
		}
	})
}

func TestDecodeBody(t *testing.T) {
	t.Run("JSON con content-type incorrecto", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(` {"a":1}`))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		var dst struct {
			A int `json:"a"`
		}
		if err := DecodeBody(r, &dst); err != nil {
			t.Fatal(err)
		}
		if dst.A != 1 {
			t.Fatalf("dst: %+v", dst)
		}
	})
	t.Run("no JSON: body intacto para ParseForm", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("a=1&b=2"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		var dst map[string]any
		if err := DecodeBody(r, &dst); err != nil {
			t.Fatal(err)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("a") != "1" || r.Form.Get("b") != "2" {
			t.Fatalf("form: %v", r.Form)
		}
	})
	t.Run("JSON inválido → error", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":`))
		var dst map[string]any
		if err := DecodeBody(r, &dst); err == nil {
			t.Fatal("esperaba error")
		}
	})
	t.Run("body nil", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Body = nil
		var dst map[string]any
		if err := DecodeBody(r, &dst); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCORS(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := CORS(next)

	t.Run("preflight 204 sin pasar por el handler", func(t *testing.T) {
		called = false
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, "/api/x", nil))
		res := rec.Result()
		defer res.Body.Close()
		if res.StatusCode != http.StatusNoContent {
			t.Fatalf("preflight: got %d", res.StatusCode)
		}
		if called {
			t.Fatal("preflight llegó al handler (pasaría por auth)")
		}
		assertCORSHeaders(t, res)
	})

	t.Run("petición normal lleva las cabeceras unión exactas", func(t *testing.T) {
		called = false
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/x", nil))
		res := rec.Result()
		defer res.Body.Close()
		if !called || res.StatusCode != http.StatusOK {
			t.Fatalf("handler: called=%v code=%d", called, res.StatusCode)
		}
		assertCORSHeaders(t, res)
	})
}

func assertCORSHeaders(t *testing.T, res *http.Response) {
	t.Helper()
	want := map[string]string{
		"Access-Control-Allow-Origin":   "*",
		"Access-Control-Allow-Methods":  "GET, POST, PUT, PATCH, DELETE, OPTIONS",
		"Access-Control-Allow-Headers":  "Authorization, Content-Type, If-Match, OCS-APIRequest",
		"Access-Control-Expose-Headers": "ETag, Last-Modified, X-Notes-API-Versions, X-Notes-Chunk-Cursor",
	}
	for k, v := range want {
		if got := res.Header.Get(k); got != v {
			t.Errorf("%s: got %q, want %q", k, got, v)
		}
	}
}

func TestPreflight(t *testing.T) {
	rec := httptest.NewRecorder()
	Preflight().ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, "/x", nil))
	if rec.Result().StatusCode != http.StatusNoContent {
		t.Fatalf("got %d", rec.Result().StatusCode)
	}
}
