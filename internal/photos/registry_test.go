package photos

import (
	"context"
	"fmt"
	"log/slog"
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
