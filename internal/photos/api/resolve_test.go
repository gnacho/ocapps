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

// TestResolve: GET /api/assets/resolve maps driveAliasAndItem to the indexed DAV
// href and returns the asset, not indexed or foreign -> {"asset": null} (same
// answer, IDOR), missing parameter -> 400.
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

	// hit: alice resolves her file (name with a space, URL escaped)
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

	// miss: path not indexed
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/assets/resolve?driveAliasAndItem="+url.QueryEscape("personal$sa/Fotos/otra.jpg"), "Bearer tok-alice")
	assertAssetNull(t, rr)

	// miss: alice asks for a path that only exists in bob's index (same name)
	// -> nothing is leaked
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/assets/resolve?driveAliasAndItem="+url.QueryEscape("personal$sb/Fotos/a.jpg"), "Bearer tok-alice")
	assertAssetNull(t, rr)

	// fileId is accepted but does not change the result (it does not index oc:id yet)
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/assets/resolve?driveAliasAndItem="+dai+"&fileId="+strconv.FormatInt(idA, 10), "Bearer tok-alice")
	if rr.Code != http.StatusOK {
		t.Fatalf("resolve with fileId -> %d", rr.Code)
	}

	// missing mandatory parameter -> 400
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/assets/resolve", "Bearer tok-alice")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("without driveAliasAndItem -> %d", rr.Code)
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
		t.Fatalf("expected a null asset, got %+v", body.Asset)
	}
}

// TestResolveNameAlias: fallback by spaceId of the fileId when the host sends
// driveAliasAndItem in the form <type>/<name>/<path> (E2E OpenCloud 8.0.1,
// gnacho/ocapps #11). Real shape observed on drive.domatix.cloud.
func TestResolveNameAlias(t *testing.T) {
	s, st, _ := newMultiServer(t)
	h := s.Handler()
	ctx := context.Background()

	idA, _, err := st.UpsertByETag(ctx, "id-alice",
		"/dav/spaces/personal$7b251a66-5d40-4f4d-b1d0-01266acea88d/Fotos/IMG_20240510_131000.jpg",
		"e1", "IMG_20240510_131000.jpg", "image", time.Now(), 100)
	if err != nil {
		t.Fatal(err)
	}

	fileID := "7d443b01-21d3-484d-bf73-a2681d670fa1$7b251a66-5d40-4f4d-b1d0-01266acea88d!41b22c35-2b5d-4998-94f6-4db877fa2993"
	dai := url.QueryEscape("personal/uitest/Fotos/IMG_20240510_131000.jpg")
	rr := do(t, h, http.MethodGet, PublicPrefix+"/api/assets/resolve?driveAliasAndItem="+dai+"&fileId="+url.QueryEscape(fileID), "Bearer tok-alice")
	if rr.Code != http.StatusOK {
		t.Fatalf("resolve -> %d", rr.Code)
	}
	var body struct {
		Asset *store.Asset `json:"asset"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Asset == nil || body.Asset.ID != idA {
		t.Fatalf("asset: %+v", body.Asset)
	}

	// miss: same shape but the spaceId of the fileId is not indexed
	otherFileID := "7d443b01-21d3-484d-bf73-a2681d670fa1$11111111-2222-3333-4444-555555555555!41b22c35-2b5d-4998-94f6-4db877fa2993"
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/assets/resolve?driveAliasAndItem="+dai+"&fileId="+url.QueryEscape(otherFileID), "Bearer tok-alice")
	assertAssetNull(t, rr)
}
