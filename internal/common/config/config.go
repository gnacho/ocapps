// Package config loads and validates the unified ocapps configuration
// (SPEC §3). Everything comes from OCAPPS_* environment variables, every new
// variable falls back to its legacy equivalent(s) with a deprecation warning
// (§3.1, §3.2). Fail-fast validation of the common part ONLY (§3.3), module
// errors are recorded per module (ModuleErr) so that the wiring degrades that
// module without taking the process down (D3).
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Defaults (SPEC §3.2).
const (
	DefaultListenAddr = "127.0.0.1:8096"
	DefaultDataDir    = "/var/lib/ocapps"
	DefaultLogLevel   = "info"
	DefaultAuthMode   = "opencloud"

	DefaultFetchTimeout  = 20 * time.Second
	DefaultFeedInterval  = 15 * time.Minute
	DefaultMaxGap        = 6 * time.Hour
	DefaultRetentionDays = 90
	DefaultNtfyURL       = "https://ntfy.sh"

	DefaultScanRoot  = "Fotos"
	DefaultScanEvery = 5 * time.Minute
)

// graphMeSuffix is the suffix that OCNOTES_GRAPH_URL carried glued to the root.
const graphMeSuffix = "/graph/v1.0/me"

// Common: configuration shared by the three modules.
type Common struct {
	OpenCloudURL string // OpenCloud server root (mandatory with AuthMode=opencloud)
	ListenAddr   string // a single listener
	DataDir      string // data root, modules hang under subdirs (§5.1)
	LogLevel     string // debug|info|warn|error
	AuthMode     string // opencloud | local (bcrypt, news only)

	NewsEnabled   bool
	NotesEnabled  bool
	PhotosEnabled bool

	// Effective data dirs per module: <DataDir>/<mod> unless the per module
	// legacy one (OCNEWS_DATA_DIR / OCNOTES_DATA_DIR / DATA_DIR) sets another
	// path (smooth transition, §3.2).
	NewsDataDir   string
	NotesDataDir  string
	PhotosDataDir string
}

// NewsConfig: news module (OCAPPS_NEWS_*).
type NewsConfig struct {
	FetchTimeout  time.Duration // > 0
	FeedInterval  time.Duration // > 0
	MaxGap        time.Duration // >= FeedInterval
	RetentionDays int           // 0 = retention disabled
	NtfyURL       string
	NtfyTopic     string
	PublicURL     string // WebSub callbacks
	AuthUser      string // bootstrap admin in local mode
	AuthPass      string
}

// Retention derives the retention duration (0 = disabled).
func (n NewsConfig) Retention() time.Duration {
	if n.RetentionDays <= 0 {
		return 0
	}
	return time.Duration(n.RetentionDays) * 24 * time.Hour
}

// NotesConfig: notes module (OCAPPS_NOTES_*).
type NotesConfig struct {
	Owner string // backfill of `user` in empty rows (migration 002)
}

// AppUser: a photos user with an app-token for the periodic background scan
// (OCAPPS_PHOTOS_USERS, H8). The app-token only lives in memory.
type AppUser struct {
	User  string
	Token string
}

// PhotosConfig: photos module (OCAPPS_PHOTOS_*). Multi-tenant (H8): there is
// no mandatory user anymore, User/AppToken remain as the legacy path for (a)
// identity of the DEPRECATED static Bearer and (b) backfill of the
// single-tenant index, and they are folded into Users for the background scan.
type PhotosConfig struct {
	User     string    // app-token user (legacy), optional since H8
	AppToken string    // OpenCloud app-token (legacy), optional since H8
	Users    []AppUser // OCAPPS_PHOTOS_USERS + legacy folding (background scan)
	Token    string    // static Bearer DEPRECATED (requires User), "" = disabled
	ScanRoot string
	// ScanSpace limits the scan to a specific drive (drive id, the one after
	// "$" in /dav/spaces/<user>$<drive>). Empty = personal drive.
	ScanSpace string
	ScanEvery time.Duration
	// CacheDir: root of the thumbnails cache. Empty = <DATA_DIR>/thumbs.
	// It is worth pointing it to a volume with room (e.g. the same one where
	// OpenCloud lives) so the container rootfs does not grow.
	CacheDir string
}

// LegacyUse records a legacy variable used at startup (§3.1).
type LegacyUse struct {
	Legacy string // legacy variable used
	New    string // equivalent OCAPPS_* variable ("" if it was removed)
}

// Config is the unified configuration (SPEC §2.1).
type Config struct {
	Common
	News   NewsConfig
	Notes  NotesConfig
	Photos PhotosConfig

	// Legacy: legacy variables in use (already logged one by one with Warn).
	Legacy []LegacyUse

	moduleErrs map[string]error
}

// ModuleErr returns the configuration error of a module
// ("news"|"notes"|"photos"), nil if its configuration is valid. An error here
// degrades the module (503), not the process (SPEC §3.3/D3).
func (c *Config) ModuleErr(module string) error {
	if c.moduleErrs == nil {
		return nil
	}
	return c.moduleErrs[module]
}

// loader applies the OCAPPS_* > legacy > default precedence policy and
// records/logs every legacy used (§3.1).
type loader struct {
	used []LegacyUse
}

// lookup: value of a LEGACY variable only if it is defined AND not empty - an
// empty legacy counts as not defined (behavior kept, §3.1).
func (l *loader) lookup(key string) (string, bool) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// lookupNew: value of a NEW OCAPPS_* variable. Unlike legacy ones, a new one
// explicitly defined as empty counts as defined (M9): it prevails over the
// legacy one, which is ignored. With the empty value, get returns "" and
// dur/intVar/boolVar fall back to the default (their raw=="" guard returns def
// with no error) - defining a new one as empty is the way to cancel a legacy
// one inherited from the environment.
func (l *loader) lookupNew(key string) (string, bool) {
	v, ok := os.LookupEnv(key)
	if !ok {
		return "", false
	}
	return v, true
}

func (l *loader) warnLegacy(legacy, nueva string) {
	for _, u := range l.used { // a single warning per variable and startup
		if u.Legacy == legacy {
			return
		}
	}
	l.used = append(l.used, LegacyUse{Legacy: legacy, New: nueva})
	if nueva == "" {
		slog.Warn("removed legacy env var, ignoring it", "legacy", legacy)
	} else {
		slog.Warn("legacy env var in use, migrate to OCAPPS_*", "legacy", legacy, "new", nueva)
	}
}

func (l *loader) alreadyUsed(legacy string) bool {
	for _, u := range l.used {
		if u.Legacy == legacy {
			return true
		}
	}
	return false
}

// get returns the value of the new var, or the legacy one if missing (with a
// warning), or def if neither exists. It records every legacy used for the
// summary log. A new one defined as empty PREVAILS over the legacy one (M9,
// see lookupNew).
// scanRootOrDefault: a variable defined but empty (a blank line in the env)
// means "not configured", not "walk the whole space". It cost a week of
// indexing on drive.domatix.cloud (issue #36): the scan walked into the
// documents tree and exhausted the deadline before reaching the photos.
// To walk the whole space it must be requested explicitly with "/".
func scanRootOrDefault(v string) string {
	if strings.TrimSpace(v) == "" {
		return DefaultScanRoot
	}
	return strings.TrimSpace(v)
}

func (l *loader) get(nueva, legacy, def string) string {
	if v, ok := l.lookupNew(nueva); ok {
		return v
	}
	if legacy != "" {
		if v, ok := l.lookup(legacy); ok {
			l.warnLegacy(legacy, nueva)
			return v
		}
	}
	return def
}

// getFirst is get with several legacy ones in precedence order among them.
func (l *loader) getFirst(nueva string, legacies []string, def string) string {
	if v, ok := l.lookupNew(nueva); ok {
		return v
	}
	for _, legacy := range legacies {
		if v, ok := l.lookup(legacy); ok {
			l.warnLegacy(legacy, nueva)
			return v
		}
	}
	return def
}

// dur parses a duration with new > legacy > def precedence. The error names
// the variable that provided the invalid value. A new one defined as empty
// blocks the legacy one and falls back to the default (M9).
func (l *loader) dur(nueva, legacy string, def time.Duration) (time.Duration, error) {
	raw, src := "", ""
	if v, ok := l.lookupNew(nueva); ok {
		raw, src = v, nueva
	} else if legacy != "" {
		if v, ok := l.lookup(legacy); ok {
			l.warnLegacy(legacy, nueva)
			raw, src = v, legacy
		}
	}
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return def, fmt.Errorf("%s invalid (%q): %w", src, raw, err)
	}
	return d, nil
}

// intVar parses an integer with new > legacy > def precedence. A new one
// defined as empty blocks the legacy one and falls back to the default (M9).
func (l *loader) intVar(nueva, legacy string, def int) (int, error) {
	raw, src := "", ""
	if v, ok := l.lookupNew(nueva); ok {
		raw, src = v, nueva
	} else if legacy != "" {
		if v, ok := l.lookup(legacy); ok {
			l.warnLegacy(legacy, nueva)
			raw, src = v, legacy
		}
	}
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def, fmt.Errorf("%s invalid (%q): %w", src, raw, err)
	}
	return n, nil
}

// boolVar parses a new boolean (no legacy). Empty = default.
func (l *loader) boolVar(nueva string, def bool) (bool, error) {
	raw, ok := l.lookupNew(nueva)
	if !ok || raw == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return def, fmt.Errorf("%s invalid (%q): %w", nueva, raw, err)
	}
	return b, nil
}

// Load reads the whole configuration. COMMON config errors -> fatal error
// (§3.3). Module errors -> kept in Config.ModuleErr(mod).
func Load() (*Config, error) {
	l := &loader{}
	c := &Config{moduleErrs: map[string]error{}}

	// --- common (§3.2) ---
	c.AuthMode = l.getFirst("OCAPPS_AUTH_MODE", []string{"OCNEWS_AUTH_MODE", "OCNOTES_AUTH_MODE"}, DefaultAuthMode)
	switch c.AuthMode {
	case "opencloud", "local":
	default:
		return nil, fmt.Errorf("invalid OCAPPS_AUTH_MODE: %q (opencloud|local)", c.AuthMode)
	}

	c.OpenCloudURL = loadOpenCloudURL(l)

	// ListenAddr: the legacy ones only detect old deployments in the logs, the
	// port is NOT inherited (each service had a different one, §3.2).
	c.ListenAddr = DefaultListenAddr
	if v, ok := l.lookupNew("OCAPPS_LISTEN_ADDR"); ok {
		c.ListenAddr = v
	} else {
		for _, legacy := range []string{"OCNEWS_ADDR", "OCNOTES_ADDR", "LISTEN_ADDR"} {
			if _, ok := l.lookup(legacy); ok {
				l.warnLegacy(legacy, "OCAPPS_LISTEN_ADDR")
				slog.Warn("legacy addr env var detected: the port is not inherited in the unified service",
					"legacy", legacy, "listen", DefaultListenAddr)
			}
		}
	}
	if c.ListenAddr == "" {
		return nil, fmt.Errorf("OCAPPS_LISTEN_ADDR cannot be empty")
	}

	c.DataDir = l.getFirst("OCAPPS_DATA_DIR", []string{"OCNEWS_DATA_DIR", "OCNOTES_DATA_DIR", "DATA_DIR"}, DefaultDataDir)
	c.NewsDataDir = moduleDataDir(l, c.DataDir, "news", "OCNEWS_DATA_DIR")
	c.NotesDataDir = moduleDataDir(l, c.DataDir, "notes", "OCNOTES_DATA_DIR")
	c.PhotosDataDir = moduleDataDir(l, c.DataDir, "photos", "DATA_DIR")

	c.LogLevel = l.get("OCAPPS_LOG_LEVEL", "OCNEWS_LOG_LEVEL", DefaultLogLevel)
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return nil, fmt.Errorf("invalid OCAPPS_LOG_LEVEL: %q (debug|info|warn|error)", c.LogLevel)
	}

	var err error
	if c.NewsEnabled, err = l.boolVar("OCAPPS_NEWS_ENABLED", true); err != nil {
		return nil, err
	}
	if c.NotesEnabled, err = l.boolVar("OCAPPS_NOTES_ENABLED", true); err != nil {
		return nil, err
	}
	if c.PhotosEnabled, err = l.boolVar("OCAPPS_PHOTOS_ENABLED", true); err != nil {
		return nil, err
	}

	// --- news (errors -> the module degrades, §3.3) ---
	c.News = NewsConfig{
		NtfyURL:   l.get("OCAPPS_NEWS_NTFY_URL", "OCNEWS_NTFY_URL", DefaultNtfyURL),
		NtfyTopic: l.get("OCAPPS_NEWS_NTFY_TOPIC", "OCNEWS_NTFY_TOPIC", ""),
		PublicURL: l.get("OCAPPS_NEWS_PUBLIC_URL", "OCNEWS_PUBLIC_URL", ""),
		AuthUser:  l.get("OCAPPS_NEWS_AUTH_USER", "AUTH_USER", ""),
		AuthPass:  l.get("OCAPPS_NEWS_AUTH_PASS", "AUTH_PASS", ""),
	}
	var newsErrs []error
	if c.News.FetchTimeout, err = l.dur("OCAPPS_NEWS_FETCH_TIMEOUT", "OCNEWS_FETCH_TIMEOUT", DefaultFetchTimeout); err != nil {
		newsErrs = append(newsErrs, err)
	} else if c.News.FetchTimeout <= 0 {
		newsErrs = append(newsErrs, fmt.Errorf("OCAPPS_NEWS_FETCH_TIMEOUT must be > 0 (got %s)", c.News.FetchTimeout))
	}
	if c.News.FeedInterval, err = l.dur("OCAPPS_NEWS_FEED_INTERVAL", "OCNEWS_FEED_INTERVAL", DefaultFeedInterval); err != nil {
		newsErrs = append(newsErrs, err)
	} else if c.News.FeedInterval <= 0 {
		newsErrs = append(newsErrs, fmt.Errorf("OCAPPS_NEWS_FEED_INTERVAL must be > 0 (got %s)", c.News.FeedInterval))
	}
	if c.News.MaxGap, err = l.dur("OCAPPS_NEWS_MAX_GAP", "OCNEWS_MAX_GAP", DefaultMaxGap); err != nil {
		newsErrs = append(newsErrs, err)
	} else if c.News.MaxGap < c.News.FeedInterval {
		newsErrs = append(newsErrs, fmt.Errorf("OCAPPS_NEWS_MAX_GAP (%s) must be >= OCAPPS_NEWS_FEED_INTERVAL (%s)", c.News.MaxGap, c.News.FeedInterval))
	}
	if c.News.RetentionDays, err = l.intVar("OCAPPS_NEWS_RETENTION_DAYS", "OCNEWS_RETENTION_DAYS", DefaultRetentionDays); err != nil {
		newsErrs = append(newsErrs, err)
	} else if c.News.RetentionDays < 0 {
		newsErrs = append(newsErrs, fmt.Errorf("OCAPPS_NEWS_RETENTION_DAYS must be >= 0 (got %d)", c.News.RetentionDays))
	}
	if len(newsErrs) > 0 {
		c.moduleErrs["news"] = errorsJoin(newsErrs...)
	}

	// --- notes ---
	c.Notes = NotesConfig{
		Owner: l.get("OCAPPS_NOTES_OWNER", "OCNOTES_OWNER", ""),
	}

	// --- photos multi-tenant (H8): there is NO mandatory user anymore (§3.3/§6.2) ---
	c.Photos = PhotosConfig{
		User:      l.get("OCAPPS_PHOTOS_USER", "OC_USER", ""),
		AppToken:  l.get("OCAPPS_PHOTOS_APP_TOKEN", "OC_APP_TOKEN", ""),
		Token:     l.get("OCAPPS_PHOTOS_TOKEN", "MEMORIES_TOKEN", ""),
		ScanRoot:  scanRootOrDefault(l.get("OCAPPS_PHOTOS_SCAN_ROOT", "SCAN_ROOT", DefaultScanRoot)),
		ScanSpace: strings.TrimSpace(l.get("OCAPPS_PHOTOS_SCAN_SPACE", "", "")),
		CacheDir:  l.get("OCAPPS_PHOTOS_CACHE_DIR", "", ""),
	}
	var photosErrs []error
	var usersErr error
	c.Photos.Users, usersErr = parsePhotosUsers(l.get("OCAPPS_PHOTOS_USERS", "", ""))
	if usersErr != nil {
		photosErrs = append(photosErrs, usersErr)
	}
	// The static Bearer is DEPRECATED and no longer has an identity of its own
	// (H8 §6.2): without OCAPPS_PHOTOS_USER there is no oc_id to map it to.
	if c.PhotosEnabled && c.Photos.Token != "" && c.Photos.User == "" {
		photosErrs = append(photosErrs, fmt.Errorf("OCAPPS_PHOTOS_TOKEN (deprecated) requires OCAPPS_PHOTOS_USER to map the token to the legacy owner"))
	}
	// Folding of the legacy USER+APP_TOKEN pair into the list of users with a
	// background scan (H8 §6.2): same effect as OCAPPS_PHOTOS_USERS.
	if c.Photos.User != "" && c.Photos.AppToken != "" && !photosUserListed(c.Photos.Users, c.Photos.User) {
		slog.Warn("OCAPPS_PHOTOS_USER + OCAPPS_PHOTOS_APP_TOKEN are deprecated for the background scan: use OCAPPS_PHOTOS_USERS",
			"user", c.Photos.User)
		c.Photos.Users = append(c.Photos.Users, AppUser{User: c.Photos.User, Token: c.Photos.AppToken})
	}
	if c.Photos.ScanEvery, err = l.dur("OCAPPS_PHOTOS_SCAN_EVERY", "SCAN_EVERY", DefaultScanEvery); err != nil {
		photosErrs = append(photosErrs, err)
	} else if c.Photos.ScanEvery <= 0 {
		photosErrs = append(photosErrs, fmt.Errorf("OCAPPS_PHOTOS_SCAN_EVERY must be > 0 (got %s)", c.Photos.ScanEvery))
	}
	if len(photosErrs) > 0 {
		c.moduleErrs["photos"] = errorsJoin(photosErrs...)
	}
	// WEB_DIR removed (legacy PWA retired, Q6): warn if it is defined.
	if _, ok := l.lookup("WEB_DIR"); ok {
		l.warnLegacy("WEB_DIR", "")
	}

	// --- fatal validation of the common part (§3.3) ---
	if c.AuthMode == "opencloud" && c.OpenCloudURL == "" {
		return nil, fmt.Errorf("OCAPPS_AUTH_MODE=opencloud requires OCAPPS_OPENCLOUD_URL (OpenCloud server root)")
	}
	if err := ensureWritableDir(c.DataDir); err != nil {
		return nil, err
	}

	c.Legacy = l.used
	return c, nil
}

// parsePhotosUsers parses OCAPPS_PHOTOS_USERS: "alice:apptoken1,bob:apptoken2"
// (users with a periodic background scan, H8). Strict format: every non-empty
// entry must be `user:token` with both parts non-empty, a malformed entry is a
// module config error (ModuleErr, pattern D3).
func parsePhotosUsers(raw string) ([]AppUser, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []AppUser
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		user, token, ok := strings.Cut(entry, ":")
		if !ok || strings.TrimSpace(user) == "" || strings.TrimSpace(token) == "" {
			return nil, fmt.Errorf("OCAPPS_PHOTOS_USERS: malformed entry %q (format: user:apptoken[,user2:apptoken2])", entry)
		}
		out = append(out, AppUser{User: strings.TrimSpace(user), Token: strings.TrimSpace(token)})
	}
	return out, nil
}

// photosUserListed: is that user already in the list (legacy folding)?
func photosUserListed(users []AppUser, user string) bool {
	for _, u := range users {
		if u.User == user {
			return true
		}
	}
	return false
}

// loadOpenCloudURL resolves the server root with the precedence
// OCAPPS_OPENCLOUD_URL > OCNEWS_OPENCOLOUD_URL (historical typo, Q1) >
// OCNOTES_GRAPH_URL (deriving the root: stripping /graph/v1.0/me) > OC_BASE_URL.
// The new one defined as empty prevails over the legacy ones (M9) and the
// fatal validation of §3.3 rejects it afterwards if the auth mode is opencloud.
func loadOpenCloudURL(l *loader) string {
	if v, ok := l.lookupNew("OCAPPS_OPENCLOUD_URL"); ok {
		return strings.TrimRight(v, "/")
	}
	if v, ok := l.lookup("OCNEWS_OPENCOLOUD_URL"); ok {
		l.warnLegacy("OCNEWS_OPENCOLOUD_URL", "OCAPPS_OPENCLOUD_URL")
		return strings.TrimRight(v, "/")
	}
	if v, ok := l.lookup("OCNOTES_GRAPH_URL"); ok {
		l.warnLegacy("OCNOTES_GRAPH_URL", "OCAPPS_OPENCLOUD_URL")
		root := strings.TrimRight(v, "/")
		if strings.HasSuffix(root, graphMeSuffix) {
			root = strings.TrimSuffix(root, graphMeSuffix)
			slog.Info("OCNOTES_GRAPH_URL pointed to /graph/v1.0/me, deriving the server root",
				"legacy", v, "root", root)
		}
		return root
	}
	if v, ok := l.lookup("OC_BASE_URL"); ok {
		l.warnLegacy("OC_BASE_URL", "OCAPPS_OPENCLOUD_URL")
		return strings.TrimRight(v, "/")
	}
	return ""
}

// moduleDataDir derives the data dir of a module: the per module legacy one if
// defined (smooth transition), otherwise <dataDir>/<module>. If the legacy one
// was already recorded as a fallback of the common DataDir the warning is not
// duplicated.
func moduleDataDir(l *loader, dataDir, module, legacy string) string {
	if v, ok := l.lookup(legacy); ok {
		if !l.alreadyUsed(legacy) {
			l.warnLegacy(legacy, "OCAPPS_DATA_DIR")
		}
		return v
	}
	return filepath.Join(dataDir, module)
}

// ensureWritableDir verifies that the common data dir can be created and
// written (fatal if not, §3.3).
func ensureWritableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("OCAPPS_DATA_DIR %q not creatable: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".ocapps-write-test-*")
	if err != nil {
		return fmt.Errorf("OCAPPS_DATA_DIR %q not writable: %w", dir, err)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}

func errorsJoin(errs ...error) error { return errors.Join(errs...) }
