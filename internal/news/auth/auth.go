// Package auth: authentication glue for the news module (SPEC §6.2). Thin by
// design: validation against Graph (5 min cache + 30 s negative cache +
// singleflight) lives in internal/common/auth, so only the news domain
// model specifics are left here:
//
//   - ShadowValidator: wraps the common Graph validator and resolves/creates the
//     local row (shadow user) by oc_id, the first user is admin.
//   - LocalValidator: bcrypt against the users table (OCAPPS_AUTH_MODE=local,
//     bootstrap/dev).
//   - Middleware + request context helpers (local user and negotiated language),
//     with the historical ocnews semantics.
//
// LAYOUT NOTE: SPEC §6.2/§8 places this glue in internal/news/auth.go
// (news root package). It lives in this one-file subpackage because api (and
// its tests) import the middleware/helpers and the news root package imports
// api (module.go), so at the root it would be an import cycle.
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

// Credential is an alias of the common type: Basic/Bearer credentials are
// extracted and validated the same way in every module (SPEC §6.1).
type Credential = commonauth.Credential

// Validator decides whether a credential is valid and resolves it to the
// LOCAL news user (shadow user in the users table).
type Validator interface {
	Validate(ctx context.Context, cred Credential) (*store.User, bool)
}

// ---------------------------------------------------------------------------
// LocalValidator: bcrypt against the users table (Basic only, local mode).
// ---------------------------------------------------------------------------

// LocalValidator validates Basic against the bcrypt hash in the users table.
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
// ShadowValidator: common Graph validator + news shadow users.
// ---------------------------------------------------------------------------

// ShadowValidator validates against OpenCloud Graph (common/auth, MultiTenant
// policy) and maintains the news users table: it resolves the shadow by
// oc_id (linking previous shadows without oc_id by username) or creates it,
// the first user in the DB is admin. Local positive cache (5 min, like the
// historical ocnews one) to avoid hitting the DB on every request, the
// negative cache (30 s) is applied by the common validator.
type ShadowValidator struct {
	graph *commonauth.GraphValidator
	store *store.Store
	log   *slog.Logger

	mu    sync.Mutex
	cache map[string]shadowEntry // basic:<user>:<sha256(pass)> / bearer:<sha256(token)> -> local userID

	upsertMu sync.Mutex // serializa upsertShadow (read-then-create)
}

type shadowEntry struct {
	userID int64
	expiry time.Time
}

const shadowCacheTTL = 5 * time.Minute

// NewShadowValidator builds the glue on top of the common Graph validator
// (already configured with the MultiTenant policy by the module wiring).
func NewShadowValidator(g *commonauth.GraphValidator, st *store.Store, log *slog.Logger) *ShadowValidator {
	if log == nil {
		log = slog.Default()
	}
	return &ShadowValidator{graph: g, store: st, log: log, cache: map[string]shadowEntry{}}
}

// Validate: local cache -> common Graph validator -> shadow user upsert.
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

// upsertShadow resolves the shadow user from the Graph identity:
// first by oc_id (canonical), if it does not exist, by username (previous
// shadows without oc_id get linked), if neither, it is created (admin if first).
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
	username := gu.Username // common: the Basic one, or SAM→UPN→mail→uuid with Bearer
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
		return nil, fmt.Errorf("create shadow: %w", err)
	}
	v.log.Info("opencloud user linked", "username", username, "display", gu.DisplayName)
	if gu.ID != "" {
		return v.store.GetUserByOCID(gu.ID)
	}
	return v.store.GetUserByUsername(username)
}

// cacheKey: same scheme as common/auth (basic:<user>:<sha256(pass)> /
// bearer:<sha256(token)>), the password is part of the key ONLY as a
// hash, never in clear text (B1: without it, any password of the same user
// got in through the cache during the TTL).
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
// Middleware + request context (local user and negotiated language).
// ---------------------------------------------------------------------------

// HashPassword computes the bcrypt hash to persist.
func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

type ctxKey int

const (
	userKey ctxKey = iota + 1
	langKey
)

// Middleware requires valid credentials (Basic or Bearer) resolved to a
// LOCAL news user, 401 otherwise. The message is negotiated by
// Accept-Language (there is no user yet).
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

// User retrieves the authenticated user from the context (nil if none).
func User(r *http.Request) *store.User {
	u, _ := r.Context().Value(userKey).(*store.User)
	return u
}

// Lang retrieves the negotiated language of the request (EN by default).
func Lang(r *http.Request) i18n.Lang {
	l, _ := r.Context().Value(langKey).(i18n.Lang)
	if l == "" {
		return i18n.EN
	}
	return l
}

// credentials extracts Basic or Bearer from the Authorization header.
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
		slog.Error("write JSON error", "err", err)
	}
}
