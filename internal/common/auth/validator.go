// Package auth: validador Graph unificado (SPEC §6). Base:
// OpenCloudValidator de ocnews (el más completo), con la caché de ocnotes y
// el chequeo single-tenant de ocphotos absorbidos como política inyectada.
//
//   - Basic user:app-token (clientes News/Notes) y Bearer <access-token>
//     (sesión web de OpenCloud) validan contra Graph /me.
//   - Caché positiva 5 min + caché NEGATIVA 30 s (un 401 no martillea el IdP)
//   - singleflight por clave (una sola petición a Graph en vuelo).
//   - La revocación de credenciales tarda ≤5 min en propagarse (ya era así
//     en news/notes; en photos pasa de 0 a 5 min — ventana aceptable, §6.1).
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// TTLs de caché (SPEC §6.1).
const (
	positiveTTL = 5 * time.Minute
	negativeTTL = 30 * time.Second
)

// Credential: exactamente una de las dos formas.
type Credential struct {
	Username, Password string // Basic
	Bearer             string // Bearer access token
}

// User es la identidad Graph resuelta (equivalente al shadow user; la
// persistencia propia de cada módulo queda en internal/<mod>).
type User struct {
	ID          string `json:"id"` // oc_id (uuid del IDM)
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
}

// Policy decide si un usuario Graph válido entra en el módulo (§6.2).
type Policy interface {
	Admit(u *User) bool
}

type multiTenant struct{}               // news, notes: todo usuario Graph válido entra
type singleTenant struct{ ocID string } // photos: solo u.ID == ocID

// MultiTenant admite a cualquier usuario válido (news, notes).
func MultiTenant() Policy { return multiTenant{} }

func (multiTenant) Admit(*User) bool { return true }

// SingleTenant admite solo al usuario con ID == ocID (photos; ocID se
// resuelve al arranque con el app-token vía webdav.MeID).
func SingleTenant(ocID string) Policy { return singleTenant{ocID: ocID} }

func (p singleTenant) Admit(u *User) bool { return u != nil && p.ocID != "" && u.ID == p.ocID }

type cacheEntry struct {
	user     *User // nil en entradas negativas
	expiry   time.Time
	negative bool
}

// GraphValidator valida credenciales contra <OCAPPS_OPENCLOUD_URL>/graph/v1.0/me.
type GraphValidator struct {
	meURL  string
	client *http.Client
	policy Policy
	log    *slog.Logger

	mu    sync.Mutex
	cache map[string]cacheEntry // basic:<user> / bearer:<sha256(token)>
	sf    singleflight.Group    // una sola llamada a Graph por clave en vuelo
}

// NewGraphValidator construye el validador. graphURL es la raíz del servidor
// OpenCloud (se le añade /graph/v1.0/me); si ya viene con ese sufijo (caso
// legacy OCNOTES_GRAPH_URL sin normalizar) se respeta.
func NewGraphValidator(graphURL string, pol Policy, log *slog.Logger) *GraphValidator {
	if pol == nil {
		pol = MultiTenant()
	}
	if log == nil {
		log = slog.Default()
	}
	u := strings.TrimRight(graphURL, "/")
	if !strings.HasSuffix(u, "/graph/v1.0/me") {
		u += "/graph/v1.0/me"
	}
	return &GraphValidator{
		meURL:  u,
		client: &http.Client{Timeout: 10 * time.Second},
		policy: pol,
		log:    log,
		cache:  map[string]cacheEntry{},
	}
}

// cacheKey: `basic:<username>` y `bearer:<sha256(token)>` (SPEC §6.1; la
// clave Basic no incluye la contraseña — la ventana de revocación es el TTL).
func cacheKey(c Credential) string {
	if c.Bearer != "" {
		h := sha256.Sum256([]byte(c.Bearer))
		return "bearer:" + hex.EncodeToString(h[:])
	}
	return "basic:" + c.Username
}

// lookup devuelve (user, negative, hit).
func (v *GraphValidator) lookup(key string) (*User, bool, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	e, ok := v.cache[key]
	if !ok || time.Now().After(e.expiry) {
		return nil, false, false
	}
	return e.user, e.negative, true
}

func (v *GraphValidator) store(key string, u *User, negative bool) {
	ttl := positiveTTL
	if negative {
		ttl = negativeTTL
	}
	v.mu.Lock()
	v.cache[key] = cacheEntry{user: u, negative: negative, expiry: time.Now().Add(ttl)}
	v.mu.Unlock()
}

// graphMe: campos de GET /graph/v1.0/me que usamos.
type graphMe struct {
	ID                       string `json:"id"`
	DisplayName              string `json:"displayName"`
	OnPremisesSamAccountName string `json:"onPremisesSamAccountName"`
	UserPrincipalName        string `json:"userPrincipalName"`
	Mail                     string `json:"mail"`
}

// fetchResult viaja por singleflight.
type fetchResult struct {
	user  *User
	valid bool
}

// Validate resuelve la credencial contra Graph (con caché + singleflight) y
// aplica la política del módulo. Devuelve (user, true) solo si la credencial
// es válida Y la política la admite.
func (v *GraphValidator) Validate(ctx context.Context, c Credential) (*User, bool) {
	if c.Bearer == "" && c.Username == "" {
		return nil, false
	}
	key := cacheKey(c)

	if u, negative, hit := v.lookup(key); hit {
		if negative || u == nil {
			return nil, false
		}
		return v.admit(u)
	}

	res, err, _ := v.sf.Do(key, func() (any, error) {
		// doble chequeo: otro vuelo pudo rellenar la caché
		if u, negative, hit := v.lookup(key); hit {
			if negative || u == nil {
				return fetchResult{}, nil
			}
			return fetchResult{user: u, valid: true}, nil
		}
		u, ok := v.fetch(ctx, c)
		v.store(key, u, !ok)
		return fetchResult{user: u, valid: ok}, nil
	})
	if err != nil {
		return nil, false
	}
	fr, ok := res.(fetchResult)
	if !ok || !fr.valid || fr.user == nil {
		return nil, false
	}
	return v.admit(fr.user)
}

func (v *GraphValidator) admit(u *User) (*User, bool) {
	if !v.policy.Admit(u) {
		return nil, false
	}
	return u, true
}

// fetch hace la petición a Graph /me y mapea la respuesta a User.
func (v *GraphValidator) fetch(ctx context.Context, c Credential) (*User, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.meURL, nil)
	if err != nil {
		return nil, false
	}
	if c.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.Bearer)
	} else {
		req.SetBasicAuth(c.Username, c.Password)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		v.log.Warn("opencloud graph /me inalcanzable", "err", err)
		return nil, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode != http.StatusUnauthorized {
			v.log.Warn("graph /me inesperado", "status", resp.StatusCode)
		}
		return nil, false
	}
	var me graphMe
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		return nil, false
	}
	return &User{
		ID:          me.ID,
		Username:    username(me, c),
		DisplayName: me.DisplayName,
		Email:       me.Mail,
	}, true
}

// username: con Basic es el del cliente; con Bearer, el account name del IDM
// (SAM), luego UPN, mail y por último el uuid (orden de ocnews/ocnotes).
func username(me graphMe, c Credential) string {
	if c.Username != "" {
		return c.Username
	}
	for _, cand := range []string{me.OnPremisesSamAccountName, me.UserPrincipalName, me.Mail, me.ID} {
		if cand != "" {
			return cand
		}
	}
	return ""
}

// String describe el validador para logs de arranque.
func (v *GraphValidator) String() string {
	return fmt.Sprintf("graph validator %s", v.meURL)
}
