// Package config carga y valida la configuración unificada de ocapps
// (SPEC §3). Todo llega por variables de entorno OCAPPS_*; cada variable
// nueva tiene fallback a su(s) equivalente(s) legacy con warning de
// deprecación (§3.1, §3.2). Validación fail-fast SOLO de lo común (§3.3);
// los errores de módulo se registran por módulo (ModuleErr) para que el
// wiring degrade ese módulo sin tumbar el proceso (D3).
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

// graphMeSuffix es el sufijo que OCNOTES_GRAPH_URL traía pegado a la raíz.
const graphMeSuffix = "/graph/v1.0/me"

// Common: configuración compartida por los tres módulos.
type Common struct {
	OpenCloudURL string // raíz del servidor OpenCloud (obligatoria con AuthMode=opencloud)
	ListenAddr   string // un solo listener
	DataDir      string // raíz de datos; los módulos cuelgan en subdirs (§5.1)
	LogLevel     string // debug|info|warn|error
	AuthMode     string // opencloud | local (bcrypt, solo news)

	NewsEnabled   bool
	NotesEnabled  bool
	PhotosEnabled bool

	// Data dirs efectivos por módulo: <DataDir>/<mod> salvo que la legacy
	// por módulo (OCNEWS_DATA_DIR / OCNOTES_DATA_DIR / DATA_DIR) defina otra
	// ruta (transición suave, §3.2).
	NewsDataDir   string
	NotesDataDir  string
	PhotosDataDir string
}

// NewsConfig: módulo news (OCAPPS_NEWS_*).
type NewsConfig struct {
	FetchTimeout  time.Duration // > 0
	FeedInterval  time.Duration // > 0
	MaxGap        time.Duration // >= FeedInterval
	RetentionDays int           // 0 = retención desactivada
	NtfyURL       string
	NtfyTopic     string
	PublicURL     string // callbacks WebSub
	AuthUser      string // bootstrap admin en modo local
	AuthPass      string
}

// Retention deriva la duración de retención (0 = desactivada).
func (n NewsConfig) Retention() time.Duration {
	if n.RetentionDays <= 0 {
		return 0
	}
	return time.Duration(n.RetentionDays) * 24 * time.Hour
}

// NotesConfig: módulo notes (OCAPPS_NOTES_*).
type NotesConfig struct {
	Owner string // backfill de `user` en filas vacías (migración 002)
}

// PhotosConfig: módulo photos (OCAPPS_PHOTOS_*).
type PhotosConfig struct {
	User      string // usuario del app-token (single-tenant); oblig. si enabled
	AppToken  string // app-token de OpenCloud; oblig. si enabled
	Token     string // Bearer estático propio (compat clientes viejos)
	ScanRoot  string
	ScanEvery time.Duration
}

// LegacyUse registra una variable legacy usada en el arranque (§3.1).
type LegacyUse struct {
	Legacy string // variable legacy usada
	New    string // variable OCAPPS_* equivalente ("" si fue eliminada)
}

// Config es la configuración unificada (SPEC §2.1).
type Config struct {
	Common
	News   NewsConfig
	Notes  NotesConfig
	Photos PhotosConfig

	// Legacy: variables legacy en uso (ya logueadas una a una con Warn).
	Legacy []LegacyUse

	moduleErrs map[string]error
}

// ModuleErr devuelve el error de configuración de un módulo
// ("news"|"notes"|"photos"), nil si su configuración es válida. Un error
// aquí degrada el módulo (503), no el proceso (SPEC §3.3/D3).
func (c *Config) ModuleErr(module string) error {
	if c.moduleErrs == nil {
		return nil
	}
	return c.moduleErrs[module]
}

// loader aplica la política de precedencia OCAPPS_* > legacy > default y
// registra/loguea cada legacy usada (§3.1).
type loader struct {
	used []LegacyUse
}

// lookup: valor de una variable LEGACY solo si está definida Y no vacía —
// una legacy vacía cuenta como no definida (comportamiento conservado, §3.1).
func (l *loader) lookup(key string) (string, bool) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// lookupNew: valor de una variable OCAPPS_* NUEVA. A diferencia de las
// legacy, una nueva definida EXPLÍCITAMENTE vacía cuenta como definida
// (M9): prevalece sobre la legacy, que se ignora. Con el valor vacío, get
// devuelve "" y dur/intVar/boolVar caen al default (su guarda raw==""
// devuelve def sin error) — definir una nueva vacía es la forma de anular
// una legacy heredada del entorno.
func (l *loader) lookupNew(key string) (string, bool) {
	v, ok := os.LookupEnv(key)
	if !ok {
		return "", false
	}
	return v, true
}

func (l *loader) warnLegacy(legacy, nueva string) {
	for _, u := range l.used { // un solo warning por variable y arranque
		if u.Legacy == legacy {
			return
		}
	}
	l.used = append(l.used, LegacyUse{Legacy: legacy, New: nueva})
	if nueva == "" {
		slog.Warn("env legacy eliminada; se ignora", "legacy", legacy)
	} else {
		slog.Warn("env legacy en uso; migra a OCAPPS_*", "legacy", legacy, "nueva", nueva)
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

// get devuelve el valor de la var nueva; si no existe, la legacy (con warn);
// si ninguna, def. Registra cada legacy usada para el log resumen. Una nueva
// definida vacía PREVALECE sobre la legacy (M9, ver lookupNew).
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

// getFirst es get con varias legacy en orden de precedencia entre ellas.
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

// dur parsea una duración con precedencia nueva > legacy > def. El error
// nombra la variable que aportó el valor inválido. Una nueva definida vacía
// bloquea la legacy y cae al default (M9).
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
		return def, fmt.Errorf("%s inválido (%q): %w", src, raw, err)
	}
	return d, nil
}

// intVar parsea un entero con precedencia nueva > legacy > def. Una nueva
// definida vacía bloquea la legacy y cae al default (M9).
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
		return def, fmt.Errorf("%s inválido (%q): %w", src, raw, err)
	}
	return n, nil
}

// boolVar parsea un booleano nuevo (sin legacy). Vacía = default.
func (l *loader) boolVar(nueva string, def bool) (bool, error) {
	raw, ok := l.lookupNew(nueva)
	if !ok || raw == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return def, fmt.Errorf("%s inválido (%q): %w", nueva, raw, err)
	}
	return b, nil
}

// Load lee la configuración completa. Errores de config COMÚN → error fatal
// (§3.3). Errores de módulo → quedan en Config.ModuleErr(mod).
func Load() (*Config, error) {
	l := &loader{}
	c := &Config{moduleErrs: map[string]error{}}

	// --- comunes (§3.2) ---
	c.AuthMode = l.getFirst("OCAPPS_AUTH_MODE", []string{"OCNEWS_AUTH_MODE", "OCNOTES_AUTH_MODE"}, DefaultAuthMode)
	switch c.AuthMode {
	case "opencloud", "local":
	default:
		return nil, fmt.Errorf("OCAPPS_AUTH_MODE inválido: %q (opencloud|local)", c.AuthMode)
	}

	c.OpenCloudURL = loadOpenCloudURL(l)

	// ListenAddr: las legacy solo detectan despliegues viejos en logs; el
	// puerto NO se hereda (cada servicio tenía uno distinto, §3.2).
	c.ListenAddr = DefaultListenAddr
	if v, ok := l.lookupNew("OCAPPS_LISTEN_ADDR"); ok {
		c.ListenAddr = v
	} else {
		for _, legacy := range []string{"OCNEWS_ADDR", "OCNOTES_ADDR", "LISTEN_ADDR"} {
			if _, ok := l.lookup(legacy); ok {
				l.warnLegacy(legacy, "OCAPPS_LISTEN_ADDR")
				slog.Warn("env legacy de addr detectada: el puerto no se hereda en el servicio unificado",
					"legacy", legacy, "listen", DefaultListenAddr)
			}
		}
	}
	if c.ListenAddr == "" {
		return nil, fmt.Errorf("OCAPPS_LISTEN_ADDR no puede estar vacío")
	}

	c.DataDir = l.getFirst("OCAPPS_DATA_DIR", []string{"OCNEWS_DATA_DIR", "OCNOTES_DATA_DIR", "DATA_DIR"}, DefaultDataDir)
	c.NewsDataDir = moduleDataDir(l, c.DataDir, "news", "OCNEWS_DATA_DIR")
	c.NotesDataDir = moduleDataDir(l, c.DataDir, "notes", "OCNOTES_DATA_DIR")
	c.PhotosDataDir = moduleDataDir(l, c.DataDir, "photos", "DATA_DIR")

	c.LogLevel = l.get("OCAPPS_LOG_LEVEL", "OCNEWS_LOG_LEVEL", DefaultLogLevel)
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return nil, fmt.Errorf("OCAPPS_LOG_LEVEL inválido: %q (debug|info|warn|error)", c.LogLevel)
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

	// --- news (errores → degrada el módulo, §3.3) ---
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
		newsErrs = append(newsErrs, fmt.Errorf("OCAPPS_NEWS_FETCH_TIMEOUT debe ser > 0 (got %s)", c.News.FetchTimeout))
	}
	if c.News.FeedInterval, err = l.dur("OCAPPS_NEWS_FEED_INTERVAL", "OCNEWS_FEED_INTERVAL", DefaultFeedInterval); err != nil {
		newsErrs = append(newsErrs, err)
	} else if c.News.FeedInterval <= 0 {
		newsErrs = append(newsErrs, fmt.Errorf("OCAPPS_NEWS_FEED_INTERVAL debe ser > 0 (got %s)", c.News.FeedInterval))
	}
	if c.News.MaxGap, err = l.dur("OCAPPS_NEWS_MAX_GAP", "OCNEWS_MAX_GAP", DefaultMaxGap); err != nil {
		newsErrs = append(newsErrs, err)
	} else if c.News.MaxGap < c.News.FeedInterval {
		newsErrs = append(newsErrs, fmt.Errorf("OCAPPS_NEWS_MAX_GAP (%s) debe ser >= OCAPPS_NEWS_FEED_INTERVAL (%s)", c.News.MaxGap, c.News.FeedInterval))
	}
	if c.News.RetentionDays, err = l.intVar("OCAPPS_NEWS_RETENTION_DAYS", "OCNEWS_RETENTION_DAYS", DefaultRetentionDays); err != nil {
		newsErrs = append(newsErrs, err)
	} else if c.News.RetentionDays < 0 {
		newsErrs = append(newsErrs, fmt.Errorf("OCAPPS_NEWS_RETENTION_DAYS debe ser >= 0 (got %d)", c.News.RetentionDays))
	}
	if len(newsErrs) > 0 {
		c.moduleErrs["news"] = errorsJoin(newsErrs...)
	}

	// --- notes ---
	c.Notes = NotesConfig{
		Owner: l.get("OCAPPS_NOTES_OWNER", "OCNOTES_OWNER", ""),
	}

	// --- photos (user/token vacíos → degrada el módulo, §3.3) ---
	c.Photos = PhotosConfig{
		User:     l.get("OCAPPS_PHOTOS_USER", "OC_USER", ""),
		AppToken: l.get("OCAPPS_PHOTOS_APP_TOKEN", "OC_APP_TOKEN", ""),
		Token:    l.get("OCAPPS_PHOTOS_TOKEN", "MEMORIES_TOKEN", ""),
		ScanRoot: l.get("OCAPPS_PHOTOS_SCAN_ROOT", "SCAN_ROOT", DefaultScanRoot),
	}
	var photosErrs []error
	if c.PhotosEnabled {
		if c.Photos.User == "" {
			photosErrs = append(photosErrs, fmt.Errorf("OCAPPS_PHOTOS_USER vacío con photos enabled"))
		}
		if c.Photos.AppToken == "" {
			photosErrs = append(photosErrs, fmt.Errorf("OCAPPS_PHOTOS_APP_TOKEN vacío con photos enabled"))
		}
	}
	if c.Photos.ScanEvery, err = l.dur("OCAPPS_PHOTOS_SCAN_EVERY", "SCAN_EVERY", DefaultScanEvery); err != nil {
		photosErrs = append(photosErrs, err)
	} else if c.Photos.ScanEvery <= 0 {
		photosErrs = append(photosErrs, fmt.Errorf("OCAPPS_PHOTOS_SCAN_EVERY debe ser > 0 (got %s)", c.Photos.ScanEvery))
	}
	if len(photosErrs) > 0 {
		c.moduleErrs["photos"] = errorsJoin(photosErrs...)
	}
	// WEB_DIR eliminada (PWA legacy retirada, Q6): si está definida, warn.
	if _, ok := l.lookup("WEB_DIR"); ok {
		l.warnLegacy("WEB_DIR", "")
	}

	// --- validación fatal de lo común (§3.3) ---
	if c.AuthMode == "opencloud" && c.OpenCloudURL == "" {
		return nil, fmt.Errorf("OCAPPS_AUTH_MODE=opencloud exige OCAPPS_OPENCLOUD_URL (raíz del servidor OpenCloud)")
	}
	if err := ensureWritableDir(c.DataDir); err != nil {
		return nil, err
	}

	c.Legacy = l.used
	return c, nil
}

// loadOpenCloudURL resuelve la raíz del servidor con la precedencia
// OCAPPS_OPENCLOUD_URL > OCNEWS_OPENCOLOUD_URL (typo histórico, Q1) >
// OCNOTES_GRAPH_URL (derivando la raíz: strip de /graph/v1.0/me) > OC_BASE_URL.
// La nueva definida vacía prevalece sobre las legacy (M9) y la validación
// fatal de §3.3 la rechaza después si auth mode es opencloud.
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
			slog.Info("OCNOTES_GRAPH_URL apuntaba a /graph/v1.0/me; se deriva la raíz del servidor",
				"legacy", v, "raiz", root)
		}
		return root
	}
	if v, ok := l.lookup("OC_BASE_URL"); ok {
		l.warnLegacy("OC_BASE_URL", "OCAPPS_OPENCLOUD_URL")
		return strings.TrimRight(v, "/")
	}
	return ""
}

// moduleDataDir deriva el data dir de un módulo: la legacy por módulo si está
// definida (transición suave), si no <dataDir>/<module>. Si la legacy ya se
// registró como fallback del DataDir común no se duplica el warning.
func moduleDataDir(l *loader, dataDir, module, legacy string) string {
	if v, ok := l.lookup(legacy); ok {
		if !l.alreadyUsed(legacy) {
			l.warnLegacy(legacy, "OCAPPS_DATA_DIR")
		}
		return v
	}
	return filepath.Join(dataDir, module)
}

// ensureWritableDir verifica que el data dir común es creable y escribible
// (fatal si no, §3.3).
func ensureWritableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("OCAPPS_DATA_DIR %q no creable: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".ocapps-write-test-*")
	if err != nil {
		return fmt.Errorf("OCAPPS_DATA_DIR %q no escribible: %w", dir, err)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}

func errorsJoin(errs ...error) error { return errors.Join(errs...) }
