// Package auth: unified Graph validator (SPEC §6). Based on the
// OpenCloudValidator from ocnews (the most complete one), with the ocnotes
// cache and the ocphotos single-tenant check absorbed as an injected policy.
//
//   - Basic user:app-token (News/Notes clients) and Bearer <access-token>
//     (OpenCloud web session) validate against Graph /me.
//   - Positive cache 5 min + NEGATIVE cache 30 s (a 401/403 from Graph does
//     not hammer the IdP, network/5xx failures are NOT cached, M7).
//   - singleflight per key (a single in-flight request to Graph).
//   - Credential revocation takes up to 5 min to propagate (it was already
//     like that in news/notes, in photos it goes from 0 to 5 min - acceptable
//     window, §6.1).
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

// Cache TTLs (SPEC §6.1).
const (
	positiveTTL = 5 * time.Minute
	negativeTTL = 30 * time.Second
)

// Credential: exactly one of the two forms.
type Credential struct {
	Username, Password string // Basic
	Bearer             string // Bearer access token
}

// User is the resolved Graph identity (equivalent to the shadow user, each
// module's own persistence lives in internal/<mod>).
type User struct {
	ID          string `json:"id"` // oc_id (uuid del IDM)
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
}

// Policy decides whether a valid Graph user enters the module (§6.2).
type Policy interface {
	Admit(u *User) bool
}

type multiTenant struct{}               // news, notes: every valid Graph user gets in
type singleTenant struct{ ocID string } // photos: only u.ID == ocID

// MultiTenant admits any valid user (news, notes).
func MultiTenant() Policy { return multiTenant{} }

func (multiTenant) Admit(*User) bool { return true }

// SingleTenant admits only the user with ID == ocID (photos, ocID is
// resolved at startup with the app-token through webdav.MeID).
func SingleTenant(ocID string) Policy { return singleTenant{ocID: ocID} }

func (p singleTenant) Admit(u *User) bool { return u != nil && p.ocID != "" && u.ID == p.ocID }

type cacheEntry struct {
	user     *User // nil on negative entries
	expiry   time.Time
	negative bool
}

// validatorState is the shareable state of a GraphValidator: cache,
// singleflight and HTTP client. Validators derived with WithPolicy share this
// state (a single Graph cache per process, SPEC §6.2) and only differ in the
// Policy applied after validation.
type validatorState struct {
	client *http.Client

	mu    sync.Mutex
	cache map[string]cacheEntry // basic:<user>:<sha256(pass)> / bearer:<sha256(token)>
	sf    singleflight.Group    // a single in-flight Graph call per key
}

// GraphValidator validates credentials against <OCAPPS_OPENCLOUD_URL>/graph/v1.0/me.
type GraphValidator struct {
	meURL  string
	policy Policy
	log    *slog.Logger
	st     *validatorState
}

// NewGraphValidator builds the validator. graphURL is the root of the OpenCloud
// server (/graph/v1.0/me is appended), if it already carries that suffix (the
// legacy OCNOTES_GRAPH_URL case, not normalized) it is kept as is.
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
		policy: pol,
		log:    log,
		st: &validatorState{
			client: &http.Client{Timeout: 10 * time.Second},
			cache:  map[string]cacheEntry{},
		},
	}
}

// WithPolicy returns a validator that shares cache, singleflight and HTTP
// client with v (a single Graph cache for the whole process, SPEC §6.2: a web
// user opening news and photos validates once against Graph) but applies
// another Policy - e.g. SingleTenant(meID) in photos versus MultiTenant in
// news/notes. The policy is evaluated AFTER the cached resolution, so sharing
// the cache does not mix tenancy between modules.
func (v *GraphValidator) WithPolicy(pol Policy) *GraphValidator {
	if pol == nil {
		pol = MultiTenant()
	}
	return &GraphValidator{meURL: v.meURL, policy: pol, log: v.log, st: v.st}
}

// cacheKey: `basic:<username>:<hex(sha256(password))>` and
// `bearer:<sha256(token)>` (SPEC §6.1). The password is part of the key ONLY
// as a hash (never in clear), restoring the semantics of the original ocnews
// (sha256("basic\x00user\x00pass")): with the key `basic:<username>` (B1),
// after a legitimate login ANY password of that user got in through the cache
// for 5 min, and a negative cache under the same key allowed a DoS of the
// legitimate user with a single failed attempt.
func cacheKey(c Credential) string {
	if c.Bearer != "" {
		h := sha256.Sum256([]byte(c.Bearer))
		return "bearer:" + hex.EncodeToString(h[:])
	}
	h := sha256.Sum256([]byte(c.Password))
	return "basic:" + c.Username + ":" + hex.EncodeToString(h[:])
}

// lookup returns (user, negative, hit).
func (v *GraphValidator) lookup(key string) (*User, bool, bool) {
	v.st.mu.Lock()
	defer v.st.mu.Unlock()
	e, ok := v.st.cache[key]
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
	v.st.mu.Lock()
	v.st.cache[key] = cacheEntry{user: u, negative: negative, expiry: time.Now().Add(ttl)}
	v.st.mu.Unlock()
}

// graphMe: fields of GET /graph/v1.0/me that we use.
type graphMe struct {
	ID                       string `json:"id"`
	DisplayName              string `json:"displayName"`
	OnPremisesSamAccountName string `json:"onPremisesSamAccountName"`
	UserPrincipalName        string `json:"userPrincipalName"`
	Mail                     string `json:"mail"`
}

// fetchResult travels through singleflight.
type fetchResult struct {
	user  *User
	valid bool
}

// Validate resolves the credential against Graph (with cache + singleflight)
// and applies the module policy. It returns (user, true) only if the
// credential is valid AND the policy admits it.
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

	res, err, _ := v.st.sf.Do(key, func() (any, error) {
		// double check: another flight may have filled the cache
		if u, negative, hit := v.lookup(key); hit {
			if negative || u == nil {
				return fetchResult{}, nil
			}
			return fetchResult{user: u, valid: true}, nil
		}
		u, ok, rejected := v.fetch(ctx, c)
		// M7: the negative cache only applies to an explicit REJECTION from
		// Graph (401/403). A network failure or a 5xx is NOT cached: the down
		// IdP must not poison the cache for 30 s and turn a glitch into an outage.
		switch {
		case ok:
			v.store(key, u, false)
		case rejected:
			v.store(key, nil, true)
		}
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

// fetch makes the request to Graph /me and maps the response to User. The
// third return value (rejected) is true only on an explicit rejection of the
// credentials (401/403): it is the only result that deserves a negative cache
// (M7), network failures, 5xx or malformed responses are not cached.
func (v *GraphValidator) fetch(ctx context.Context, c Credential) (*User, bool, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.meURL, nil)
	if err != nil {
		return nil, false, false
	}
	if c.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.Bearer)
	} else {
		req.SetBasicAuth(c.Username, c.Password)
	}
	resp, err := v.st.client.Do(req)
	if err != nil {
		v.log.Warn("opencloud graph /me unreachable", "err", err)
		return nil, false, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		rejected := resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden
		if !rejected {
			v.log.Warn("unexpected graph /me", "status", resp.StatusCode)
		}
		return nil, false, rejected
	}
	var me graphMe
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		return nil, false, false
	}
	return &User{
		ID:          me.ID,
		Username:    username(me, c),
		DisplayName: me.DisplayName,
		Email:       me.Mail,
	}, true, false
}

// username: with Basic it is the client one, with Bearer, the IDM account
// name (SAM), then UPN, mail and finally the uuid (ocnews/ocnotes order).
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

// String describes the validator for startup logs.
func (v *GraphValidator) String() string {
	return fmt.Sprintf("graph validator %s", v.meURL)
}
