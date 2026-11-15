package auth

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/gnacho/ocapps/internal/common/httpx"
)

type ctxKey int

const userKey ctxKey = iota + 1

// Middleware requires valid credentials (Basic or Bearer) validated against
// Graph, 401 with WWW-Authenticate otherwise. The authenticated *User is kept
// in the request context (retrieve it with FromContext).
func Middleware(v *GraphValidator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred, ok := credentials(r)
		var u *User
		if ok {
			u, ok = v.Validate(r.Context(), cred)
		}
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="ocapps"`)
			httpx.ErrorStatus(w, r, httpx.NewError(http.StatusUnauthorized, "unauthorized", "invalid credentials"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	})
}

// FromContext retrieves the authenticated user from the context (nil if absent).
func FromContext(ctx context.Context) *User {
	u, _ := ctx.Value(userKey).(*User)
	return u
}

// User is a shorthand for FromContext on the request.
func UserFrom(r *http.Request) *User { return FromContext(r.Context()) }

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
