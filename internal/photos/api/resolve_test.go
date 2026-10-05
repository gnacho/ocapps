package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/gnacho/ocapps/internal/photos/store"
)

// TestResolve: GET /api/assets/resolve mapea driveAliasAndItem al href DAV
// indexado y devuelve el asset; no indexado o ajeno -> {"asset": null}
// (misma respuesta, IDOR); falta el parámetro -> 400.
func TestResolve(t *testing.T) {
	s, st, _ := newMultiServer(t)
	h := s.Handler()
	ctx := context.Background()

	idA, _, err := st.UpsertByETag(ctx, "id-alice", "/dav/spaces/personal$sa/Fotos/la playa.jpg", "e1", "la playa.jpg", "image", time.Now(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.UpsertByETag(ctx, "id-bob", "/dav/spaces/personal$sb/Fotos/a.jpg", "e2", "a.jpg", "image", time.Now(), 100); err != nil {
		t.Fatal(err)
	}

	// hit: alice resuelve su fichero (nombre con espacio, URL-escapado)
	dai := url.QueryEscape("personal$sa/Fotos/la playa.jpg")
	rr := do(t, h, http.MethodGet, PublicPrefix+"/api/assets/resolve?driveAliasAndItem="+dai, "Bearer tok-alice")
	if rr.Code != http.StatusOK {
		t.Fatalf("resolve -> %d", rr.Code)
	}
	var body struct {
		Asset *store.Asset `json:"asset"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Asset == nil || body.Asset.ID != idA || body.Asset.Filename != "la playa.jpg" {
		t.Fatalf("asset: %+v", body.Asset)
	}

	// miss: path no indexado
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/assets/resolve?driveAliasAndItem="+url.QueryEscape("personal$sa/Fotos/otra.jpg"), "Bearer tok-alice")
	assertAssetNull(t, rr)

	// miss: alice pregunta por un path que solo existe en el índice de bob
	// (mismo nombre) -> no delata nada
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/assets/resolve?driveAliasAndItem="+url.QueryEscape("personal$sb/Fotos/a.jpg"), "Bearer tok-alice")
	assertAssetNull(t, rr)

	// fileId se acepta pero no cambia el resultado (aún no indexa oc:id)
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/assets/resolve?driveAliasAndItem="+dai+"&fileId="+strconv.FormatInt(idA, 10), "Bearer tok-alice")
	if rr.Code != http.StatusOK {
		t.Fatalf("resolve con fileId -> %d", rr.Code)
	}

	// falta el parámetro obligatorio -> 400
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/assets/resolve", "Bearer tok-alice")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("sin driveAliasAndItem -> %d", rr.Code)
	}
}

func assertAssetNull(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("miss -> %d", rr.Code)
	}
	var body struct {
		Asset *store.Asset `json:"asset"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Asset != nil {
		t.Fatalf("se esperaba asset null, got %+v", body.Asset)
	}
}
