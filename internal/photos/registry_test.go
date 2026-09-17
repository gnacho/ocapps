package photos

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gnacho/ocapps/internal/common/auth"
	"github.com/gnacho/ocapps/internal/common/config"
)

// TestRegistryConcurrenciaTouchDavFor: Touch (escritor) contra DavFor/get/
// SpacePrefix/scanOne (lectores) — el race del review H8 (sess.dav leído
// fuera del mutex). Debe correr con -race.
func TestRegistryConcurrenciaTouchDavFor(t *testing.T) {
	srv := fakeOpenCloud(t, propfindUnaFoto)
	m, err := New(testConfig(config.Common{OpenCloudURL: srv.URL, PhotosDataDir: t.TempDir()},
		config.PhotosConfig{ScanEvery: time.Hour}), slog.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(2)
		go func(w int) { // escritores: Touch con tokens cambiantes
			defer wg.Done()
			for i := 0; i < 200; i++ {
				m.reg.Touch(ctx, &auth.User{ID: "user-1"}, auth.Credential{Bearer: fmt.Sprintf("tok-%d-%d", w, i)})
			}
		}(w)
		go func() { // lectores: DavFor + get + SpacePrefix + resolveSpace (como scanOne)
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

// TestTouchNoDegradaSesionSembrada: un usuario de OCAPPS_PHOTOS_USERS que
// abre la web (Touch con Bearer OIDC efímero) NO pierde su app-token: el
// background scan del ticker sigue usando Basic tras caducar el Bearer
// (H8 review FIX 2).
func TestTouchNoDegradaSesionSembrada(t *testing.T) {
	// OpenCloud cuyo PROPFIND SOLO acepta el app-token Basic (el Bearer
	// caducado recibe 401): si el scan usara el Bearer, fallaría.
	srv := fakeOpenCloudReq(t, func(r *http.Request) (int, string) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "admin" || p != "app-token" {
			return http.StatusUnauthorized, ""
		}
		return propfindUnaFoto(r.URL.Path)
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
		t.Fatal("sin dav tras sembrar")
	}
	// el usuario abre la web: Touch con su Bearer OIDC
	m.reg.Touch(ctx, &auth.User{ID: owner}, auth.Credential{Bearer: "oidc-efimero"})

	// la sesión conserva el cliente Basic con app-token
	sess := m.reg.get(owner)
	if sess == nil || !sess.isBasic || !sess.seeded {
		t.Fatalf("sesión tras Touch Bearer: %+v, debe seguir sembrada y Basic", sess)
	}
	if got := m.reg.DavFor(ctx, owner); got != davSembrado {
		t.Fatal("Touch reemplazó el dav sembrado por el Bearer efímero")
	}
	// y el scan (ticker) sigue indexando con el app-token
	m.sched.scanOne(ctx, owner)
	stats, err := m.st.Stats(ctx, owner)
	if err != nil || stats["assets"].(int64) != 1 {
		t.Fatalf("scan con app-token tras Touch Bearer: %v %v", stats, err)
	}
}

// TestDavForOcultaSesionesCaducadas: DavFor devuelve nil para una sesión
// Bearer marcada como caducada (401/403 detectado por un scan) hasta que el
// próximo Touch del usuario la refresca con su token nuevo (H8 review).
func TestDavForOcultaSesionesCaducadas(t *testing.T) {
	m, err := New(testConfig(config.Common{OpenCloudURL: "http://127.0.0.1:1", PhotosDataDir: t.TempDir()},
		config.PhotosConfig{ScanEvery: time.Hour}), slog.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m.reg.Touch(ctx, &auth.User{ID: "user-1"}, auth.Credential{Bearer: "tok-1"})
	if m.reg.DavFor(ctx, "user-1") == nil {
		t.Fatal("dav esperado tras Touch")
	}
	m.reg.markExpired("user-1")
	if m.reg.DavFor(ctx, "user-1") != nil {
		t.Fatal("una sesión caducada no debe servir su dav")
	}
	// el próximo request del usuario (Touch con token nuevo) la recupera
	m.reg.Touch(ctx, &auth.User{ID: "user-1"}, auth.Credential{Bearer: "tok-2"})
	if m.reg.DavFor(ctx, "user-1") == nil {
		t.Fatal("Touch con token fresco debe recuperar la sesión")
	}
}
