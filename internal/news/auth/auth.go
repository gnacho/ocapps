// Package auth: glue de autenticación del módulo news (SPEC §6.2). Fino por
// diseño: la validación contra Graph (caché 5 min + negativa 30 s +
// singleflight) vive en internal/common/auth; aquí solo queda lo específico
// del modelo de dominio de news:
//
//   - ShadowValidator: envuelve el validador Graph común y resuelve/crea la
//     fila local (shadow user) por oc_id; el primer usuario es admin.
//   - LocalValidator: bcrypt contra la tabla users (OCAPPS_AUTH_MODE=local,
//     bootstrap/dev).
//   - Middleware + helpers de contexto (usuario local e idioma negociado),
//     con la semántica histórica de ocnews.
//
// NOTA de layout: el SPEC §6.2/§8 ubica este glue en internal/news/auth.go
// (paquete raíz news). Vive en este subpaquete de un fichero porque api (y
// sus tests) importan el middleware/helpers y el paquete raíz news importa
// api (module.go): en la raíz sería un ciclo de imports.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	commonauth "github.com/gnacho/ocapps/internal/common/auth"
	"github.com/gnacho/ocapps/internal/news/i18n"
	"github.com/gnacho/ocapps/internal/news/store"
)

// Credential es un alias del tipo común: las credenciales Basic/Bearer se
// extraen y validan igual en todos los módulos (SPEC §6.1).
type Credential = commonauth.Credential

// Validator decide si una credencial es válida y la resuelve al usuario
// LOCAL de news (shadow user de la tabla users).
type Validator interface {
	Validate(ctx context.Context, cred Credential) (*store.User, bool)
}

// ---------------------------------------------------------------------------
// LocalValidator: bcrypt contra la tabla users (solo Basic; modo local).
// ---------------------------------------------------------------------------

// LocalValidator valida Basic contra el hash bcrypt de la tabla users.
type LocalValidator struct {
	Store *store.Store
}

func (l *LocalValidator) Validate(_ context.Context, cred Credential) (*store.User, bool) {
	if cred.Bearer != "" || cred.Username == "" {
		return nil, false
	}
	u, err := l.Store.GetUserByUsername(cred.Username)
	if err != nil {
		return nil, false
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(cred.Password)) != nil {
		return nil, false
	}
	l.Store.TouchLogin(u.ID)
	return u, true
}

// ---------------------------------------------------------------------------
// ShadowValidator: validador Graph común + shadow users de news.
// ---------------------------------------------------------------------------

// ShadowValidator valida contra OpenCloud Graph (common/auth, política
// MultiTenant) y mantiene la tabla users de news: resuelve el shadow por
// oc_id (vinculando sombras previas sin oc_id por username) o lo crea; el
// primer usuario de la BD es admin. Caché positiva local (5 min, como la
// histórica de ocnews) para no tocar la BD en cada petición; la caché
// negativa (30 s) la aplica el validador común.
type ShadowValidator struct {
	graph *commonauth.GraphValidator
	store *store.Store
	log   *slog.Logger

	mu    sync.Mutex
	cache map[string]shadowEntry // basic:<user>:<sha256(pass)> / bearer:<sha256(token)> → userID local

	upsertMu sync.Mutex // serializa upsertShadow (read-then-create)
}

type shadowEntry struct {
	userID int64
	expiry time.Time
}

const shadowCacheTTL = 5 * time.Minute

// NewShadowValidator construye el glue sobre el validador Graph común
// (ya configurado con la política MultiTenant por el wiring del módulo).
func NewShadowValidator(g *commonauth.GraphValidator, st *store.Store, log *slog.Logger) *ShadowValidator {
	if log == nil {
		log = slog.Default()
	}
	return &ShadowValidator{graph: g, store: st, log: log, cache: map[string]shadowEntry{}}
}

// Validate: caché local → validador Graph común → upsert del shadow user.
func (v *ShadowValidator) Validate(ctx context.Context, cred Credential) (*store.User, bool) {
	if cred.Bearer == "" && cred.Username == "" {
		return nil, false
	}
	key := cacheKey(cred)

	v.mu.Lock()
	if e, ok := v.cache[key]; ok && time.Now().Before(e.expiry) {
		v.mu.Unlock()
		if u, err := v.store.GetUserByID(e.userID); err == nil {
			return u, true
		}
	} else {
		v.mu.Unlock()
	}

	gu, ok := v.graph.Validate(ctx, cred)
	if !ok || gu == nil {
		return nil, false
	}
	u, err := v.upsertShadow(gu)
	if err != nil {
		v.log.Error("shadow user", "err", err)
		return nil, false
	}
	v.store.TouchLogin(u.ID)

	v.mu.Lock()
	v.cache[key] = shadowEntry{userID: u.ID, expiry: time.Now().Add(shadowCacheTTL)}
	v.mu.Unlock()
	return u, true
}

// upsertShadow resuelve el shadow user a partir de la identidad Graph:
// primero por oc_id (canónico); si no existe, por username (sombras previas
// sin oc_id se vinculan); si tampoco, se crea (admin si es el primero).
func (v *ShadowValidator) upsertShadow(gu *commonauth.User) (*store.User, error) {
	v.upsertMu.Lock()
	defer v.upsertMu.Unlock()

	if gu.ID != "" {
		if u, err := v.store.GetUserByOCID(gu.ID); err == nil {
			if gu.DisplayName != "" && u.DisplayName != gu.DisplayName {
				_ = v.store.UpdateProfile(u.ID, gu.DisplayName, u.Language)
			}
			return v.store.GetUserByOCID(gu.ID)
		}
	}
	username := gu.Username // común: el del Basic, o SAM→UPN→mail→uuid con Bearer
	if u, err := v.store.GetUserByUsername(username); err == nil && gu.ID != "" {
		if u.OCID == "" {
			_ = v.store.SetUserOCID(u.ID, gu.ID)
		}
		if gu.DisplayName != "" && u.DisplayName != gu.DisplayName {
			_ = v.store.UpdateProfile(u.ID, gu.DisplayName, u.Language)
		}
		return v.store.GetUserByUsername(username)
	}
	n, err := bcrypt.GenerateFromPassword([]byte(randomShadowHash()), bcrypt.MinCost)
	if err != nil {
		return nil, err
	}
	role := "user"
	if users, err := v.store.CountUsers(); err == nil && users == 0 {
		role = "admin"
	}
	if _, err := v.store.CreateUserWithOCID(username, gu.ID, "shadow:"+string(n), gu.DisplayName, role); err != nil {
		if errors.Is(err, store.ErrConflict) {
			if gu.ID != "" {
				if u, e := v.store.GetUserByOCID(gu.ID); e == nil {
					return u, nil
				}
			}
			if u, e := v.store.GetUserByUsername(username); e == nil {
				return u, nil
			}
		}
		return nil, fmt.Errorf("crear shadow: %w", err)
	}
	v.log.Info("usuario opencloud vinculado", "username", username, "display", gu.DisplayName)
	if gu.ID != "" {
		return v.store.GetUserByOCID(gu.ID)
	}
	return v.store.GetUserByUsername(username)
}

// cacheKey: mismo esquema que common/auth (basic:<user>:<sha256(pass)> /
// bearer:<sha256(token)>) — la contraseña forma parte de la clave SOLO como
// hash, nunca en claro (B1: sin ella, cualquier password del mismo usuario
// entraba por caché durante el TTL).
func cacheKey(cred Credential) string {
	if cred.Bearer != "" {
		h := sha256.Sum256([]byte(cred.Bearer))
		return "bearer:" + hex.EncodeToString(h[:])
	}
	h := sha256.Sum256([]byte(cred.Password))
	return "basic:" + cred.Username + ":" + hex.EncodeToString(h[:])
}

func randomShadowHash() string {
	b := make([]byte, 8)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ---------------------------------------------------------------------------
// Middleware + contexto de petición (usuario local e idioma negociado).
// ---------------------------------------------------------------------------

// HashPassword calcula el hash bcrypt para persistir.
func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

type ctxKey int

const (
	userKey ctxKey = iota + 1
	langKey
)

// Middleware exige credenciales válidas (Basic o Bearer) resueltas a un
// usuario LOCAL de news; 401 si no. El mensaje se negocia por
// Accept-Language (no hay usuario todavía).
func Middleware(v Validator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred, ok := credentials(r)
		var u *store.User
		if ok {
			u, ok = v.Validate(r.Context(), cred)
		}
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="ocnews"`)
			lang := i18n.Negotiate("auto", r.Header.Get("Accept-Language"))
			writeError(w, http.StatusUnauthorized, "unauthorized", i18n.T(lang, "unauthorized"))
			return
		}
		ctx := context.WithValue(r.Context(), userKey, u)
		ctx = context.WithValue(ctx, langKey, i18n.Negotiate(u.Language, r.Header.Get("Accept-Language")))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// User recupera el usuario autenticado del contexto (nil si no hay).
func User(r *http.Request) *store.User {
	u, _ := r.Context().Value(userKey).(*store.User)
	return u
}

// Lang recupera el idioma negociado de la petición (EN por defecto).
func Lang(r *http.Request) i18n.Lang {
	l, _ := r.Context().Value(langKey).(i18n.Lang)
	if l == "" {
		return i18n.EN
	}
	return l
}

// credentials extrae Basic o Bearer de la cabecera Authorization.
func credentials(r *http.Request) (Credential, bool) {
	h := r.Header.Get("Authorization")
	switch {
	case strings.HasPrefix(h, "Basic "):
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Basic "))
		if err != nil {
			return Credential{}, false
		}
		user, pass, ok := strings.Cut(string(raw), ":")
		if !ok || user == "" {
			return Credential{}, false
		}
		return Credential{Username: user, Password: pass}, true
	case strings.HasPrefix(h, "Bearer "):
		tok := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		if tok == "" {
			return Credential{}, false
		}
		return Credential{Bearer: tok}, true
	}
	return Credential{}, false
}

type errBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	var b errBody
	b.Error.Code = code
	b.Error.Message = msg
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(b); err != nil {
		slog.Error("escribir error JSON", "err", err)
	}
}
