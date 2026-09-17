package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// allVars: todas las variables del esquema §3.2 (nuevas y legacy). Cada test
// las DESDEFINE para aislar el caso (M9: una OCAPPS_* definida vacía ya NO
// equivale a no definida — blanquear con Setenv(k, "") no sirve).
var allVars = []string{
	"OCAPPS_OPENCLOUD_URL", "OCNEWS_OPENCOLOUD_URL", "OCNOTES_GRAPH_URL", "OC_BASE_URL",
	"OCAPPS_LISTEN_ADDR", "OCNEWS_ADDR", "OCNOTES_ADDR", "LISTEN_ADDR",
	"OCAPPS_DATA_DIR", "OCNEWS_DATA_DIR", "OCNOTES_DATA_DIR", "DATA_DIR",
	"OCAPPS_LOG_LEVEL", "OCNEWS_LOG_LEVEL",
	"OCAPPS_AUTH_MODE", "OCNEWS_AUTH_MODE", "OCNOTES_AUTH_MODE",
	"OCAPPS_NEWS_ENABLED", "OCAPPS_NOTES_ENABLED", "OCAPPS_PHOTOS_ENABLED",
	"OCAPPS_NEWS_FETCH_TIMEOUT", "OCNEWS_FETCH_TIMEOUT",
	"OCAPPS_NEWS_FEED_INTERVAL", "OCNEWS_FEED_INTERVAL",
	"OCAPPS_NEWS_MAX_GAP", "OCNEWS_MAX_GAP",
	"OCAPPS_NEWS_RETENTION_DAYS", "OCNEWS_RETENTION_DAYS",
	"OCAPPS_NEWS_NTFY_URL", "OCNEWS_NTFY_URL",
	"OCAPPS_NEWS_NTFY_TOPIC", "OCNEWS_NTFY_TOPIC",
	"OCAPPS_NEWS_PUBLIC_URL", "OCNEWS_PUBLIC_URL",
	"OCAPPS_NEWS_AUTH_USER", "OCAPPS_NEWS_AUTH_PASS", "AUTH_USER", "AUTH_PASS",
	"OCAPPS_NOTES_OWNER", "OCNOTES_OWNER",
	"OCAPPS_PHOTOS_USER", "OC_USER",
	"OCAPPS_PHOTOS_APP_TOKEN", "OC_APP_TOKEN",
	"OCAPPS_PHOTOS_USERS",
	"OCAPPS_PHOTOS_TOKEN", "MEMORIES_TOKEN",
	"OCAPPS_PHOTOS_SCAN_ROOT", "SCAN_ROOT",
	"OCAPPS_PHOTOS_SCAN_EVERY", "SCAN_EVERY",
	"WEB_DIR",
}

// unsetEnv DESDEFINE cada variable y restaura su valor previo al terminar
// el test (equivalente a t.Setenv pero para "no definida").
func unsetEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		k := k
		v, ok := os.LookupEnv(k)
		t.Cleanup(func() {
			if ok {
				_ = os.Setenv(k, v)
			} else {
				_ = os.Unsetenv(k)
			}
		})
		_ = os.Unsetenv(k)
	}
}

// base prepara un entorno limpio y válido (auth opencloud + data dir propio).
func base(t *testing.T) {
	t.Helper()
	unsetEnv(t, allVars...)
	t.Setenv("OCAPPS_OPENCLOUD_URL", "https://cloud.example.com")
	t.Setenv("OCAPPS_DATA_DIR", t.TempDir())
	t.Setenv("OCAPPS_PHOTOS_USER", "alice")
	t.Setenv("OCAPPS_PHOTOS_APP_TOKEN", "tok")
}

func legacyUsed(cfg *Config, legacy string) bool {
	for _, u := range cfg.Legacy {
		if u.Legacy == legacy {
			return true
		}
	}
	return false
}

func TestDefaults(t *testing.T) {
	base(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OpenCloudURL != "https://cloud.example.com" {
		t.Errorf("OpenCloudURL: %q", cfg.OpenCloudURL)
	}
	if cfg.ListenAddr != "127.0.0.1:8096" {
		t.Errorf("ListenAddr: %q", cfg.ListenAddr)
	}
	if cfg.LogLevel != "info" || cfg.AuthMode != "opencloud" {
		t.Errorf("LogLevel/AuthMode: %q %q", cfg.LogLevel, cfg.AuthMode)
	}
	if !cfg.NewsEnabled || !cfg.NotesEnabled || !cfg.PhotosEnabled {
		t.Error("enabled por defecto debe ser true x3")
	}
	// subdirs por módulo (§5.1)
	for m, dir := range map[string]string{"news": cfg.NewsDataDir, "notes": cfg.NotesDataDir, "photos": cfg.PhotosDataDir} {
		if !strings.HasSuffix(dir, string([]byte{'/'})+m) {
			t.Errorf("%sDataDir: %q", m, dir)
		}
	}
	if cfg.News.FetchTimeout != 20*time.Second || cfg.News.FeedInterval != 15*time.Minute ||
		cfg.News.MaxGap != 6*time.Hour || cfg.News.RetentionDays != 90 ||
		cfg.News.NtfyURL != "https://ntfy.sh" || cfg.News.NtfyTopic != "" || cfg.News.PublicURL != "" ||
		cfg.News.AuthUser != "" || cfg.News.AuthPass != "" {
		t.Errorf("news defaults: %+v", cfg.News)
	}
	if cfg.News.Retention() != 90*24*time.Hour {
		t.Errorf("Retention(): %s", cfg.News.Retention())
	}
	if cfg.Notes.Owner != "" {
		t.Errorf("notes owner: %q", cfg.Notes.Owner)
	}
	if cfg.Photos.User != "alice" || cfg.Photos.ScanRoot != "Fotos" || cfg.Photos.ScanEvery != 5*time.Minute ||
		cfg.Photos.Token != "" {
		t.Errorf("photos defaults: %+v", cfg.Photos)
	}
	for _, m := range []string{"news", "notes", "photos"} {
		if err := cfg.ModuleErr(m); err != nil {
			t.Errorf("ModuleErr(%s): %v", m, err)
		}
	}
	if len(cfg.Legacy) != 0 {
		t.Errorf("sin legacy esperada: %+v", cfg.Legacy)
	}
}

// TestPrecedenciaNuevaSobreLegacy: para cada fila de §3.2 con legacy, el
// valor OCAPPS_* gana al legacy, el legacy gana al default, y usar legacy
// queda registrado (warning §3.1).
func TestPrecedenciaNuevaSobreLegacy(t *testing.T) {
	cases := []struct {
		name      string
		nueva     string
		legacy    string
		setNueva  string // "" = no se define la nueva
		setLegacy string
		check     func(t *testing.T, cfg *Config)
	}{
		{"opencloud_url nueva gana", "OCAPPS_OPENCLOUD_URL", "OCNEWS_OPENCOLOUD_URL",
			"https://nueva.example.com", "https://legacy.example.com",
			func(t *testing.T, c *Config) {
				if c.OpenCloudURL != "https://nueva.example.com" {
					t.Errorf("got %q", c.OpenCloudURL)
				}
				if legacyUsed(c, "OCNEWS_OPENCOLOUD_URL") {
					t.Error("legacy registrada sin usarse")
				}
			}},
		{"opencloud_url legacy typo", "OCAPPS_OPENCLOUD_URL", "OCNEWS_OPENCOLOUD_URL",
			"", "https://legacy.example.com/",
			func(t *testing.T, c *Config) {
				if c.OpenCloudURL != "https://legacy.example.com" { // trailing / recortada
					t.Errorf("got %q", c.OpenCloudURL)
				}
				if !legacyUsed(c, "OCNEWS_OPENCOLOUD_URL") {
					t.Error("legacy typo no registrada")
				}
			}},
		{"oc_base_url legacy", "OCAPPS_OPENCLOUD_URL", "OC_BASE_URL",
			"", "https://photos-legacy.example.com",
			func(t *testing.T, c *Config) {
				if c.OpenCloudURL != "https://photos-legacy.example.com" {
					t.Errorf("got %q", c.OpenCloudURL)
				}
				if !legacyUsed(c, "OC_BASE_URL") {
					t.Error("OC_BASE_URL no registrada")
				}
			}},
		{"log_level legacy", "OCAPPS_LOG_LEVEL", "OCNEWS_LOG_LEVEL",
			"", "debug",
			func(t *testing.T, c *Config) {
				if c.LogLevel != "debug" || !legacyUsed(c, "OCNEWS_LOG_LEVEL") {
					t.Errorf("got %q legacy=%v", c.LogLevel, c.Legacy)
				}
			}},
		{"auth_mode legacy notes", "OCAPPS_AUTH_MODE", "OCNOTES_AUTH_MODE",
			"", "local",
			func(t *testing.T, c *Config) {
				if c.AuthMode != "local" || !legacyUsed(c, "OCNOTES_AUTH_MODE") {
					t.Errorf("got %q legacy=%v", c.AuthMode, c.Legacy)
				}
			}},
		{"fetch_timeout legacy", "OCAPPS_NEWS_FETCH_TIMEOUT", "OCNEWS_FETCH_TIMEOUT",
			"", "7s",
			func(t *testing.T, c *Config) {
				if c.News.FetchTimeout != 7*time.Second || !legacyUsed(c, "OCNEWS_FETCH_TIMEOUT") {
					t.Errorf("got %s legacy=%v", c.News.FetchTimeout, c.Legacy)
				}
			}},
		{"feed_interval legacy", "OCAPPS_NEWS_FEED_INTERVAL", "OCNEWS_FEED_INTERVAL",
			"", "3m",
			func(t *testing.T, c *Config) {
				if c.News.FeedInterval != 3*time.Minute || !legacyUsed(c, "OCNEWS_FEED_INTERVAL") {
					t.Errorf("got %s", c.News.FeedInterval)
				}
			}},
		{"max_gap legacy", "OCAPPS_NEWS_MAX_GAP", "OCNEWS_MAX_GAP",
			"", "12h",
			func(t *testing.T, c *Config) {
				if c.News.MaxGap != 12*time.Hour || !legacyUsed(c, "OCNEWS_MAX_GAP") {
					t.Errorf("got %s", c.News.MaxGap)
				}
			}},
		{"retention legacy", "OCAPPS_NEWS_RETENTION_DAYS", "OCNEWS_RETENTION_DAYS",
			"", "30",
			func(t *testing.T, c *Config) {
				if c.News.RetentionDays != 30 || c.News.Retention() != 30*24*time.Hour ||
					!legacyUsed(c, "OCNEWS_RETENTION_DAYS") {
					t.Errorf("got %d", c.News.RetentionDays)
				}
			}},
		{"retention 0 desactiva", "OCAPPS_NEWS_RETENTION_DAYS", "OCNEWS_RETENTION_DAYS",
			"0", "30",
			func(t *testing.T, c *Config) {
				if c.News.RetentionDays != 0 || c.News.Retention() != 0 {
					t.Errorf("retención no desactivada: %+v", c.News)
				}
			}},
		{"ntfy_url legacy", "OCAPPS_NEWS_NTFY_URL", "OCNEWS_NTFY_URL",
			"", "https://ntfy.example.com",
			func(t *testing.T, c *Config) {
				if c.News.NtfyURL != "https://ntfy.example.com" || !legacyUsed(c, "OCNEWS_NTFY_URL") {
					t.Errorf("got %q", c.News.NtfyURL)
				}
			}},
		{"ntfy_topic legacy", "OCAPPS_NEWS_NTFY_TOPIC", "OCNEWS_NTFY_TOPIC",
			"", "mi-topic",
			func(t *testing.T, c *Config) {
				if c.News.NtfyTopic != "mi-topic" || !legacyUsed(c, "OCNEWS_NTFY_TOPIC") {
					t.Errorf("got %q", c.News.NtfyTopic)
				}
			}},
		{"public_url legacy", "OCAPPS_NEWS_PUBLIC_URL", "OCNEWS_PUBLIC_URL",
			"", "https://cloud.example.com/news",
			func(t *testing.T, c *Config) {
				if c.News.PublicURL != "https://cloud.example.com/news" || !legacyUsed(c, "OCNEWS_PUBLIC_URL") {
					t.Errorf("got %q", c.News.PublicURL)
				}
			}},
		{"auth_user legacy genérica", "OCAPPS_NEWS_AUTH_USER", "AUTH_USER",
			"", "admin",
			func(t *testing.T, c *Config) {
				if c.News.AuthUser != "admin" || !legacyUsed(c, "AUTH_USER") {
					t.Errorf("got %q", c.News.AuthUser)
				}
			}},
		{"auth_pass legacy genérica", "OCAPPS_NEWS_AUTH_PASS", "AUTH_PASS",
			"", "s3cret",
			func(t *testing.T, c *Config) {
				if c.News.AuthPass != "s3cret" || !legacyUsed(c, "AUTH_PASS") {
					t.Errorf("got %q", c.News.AuthPass)
				}
			}},
		{"notes owner legacy", "OCAPPS_NOTES_OWNER", "OCNOTES_OWNER",
			"", "alice",
			func(t *testing.T, c *Config) {
				if c.Notes.Owner != "alice" || !legacyUsed(c, "OCNOTES_OWNER") {
					t.Errorf("got %q", c.Notes.Owner)
				}
			}},
		{"photos user legacy", "OCAPPS_PHOTOS_USER", "OC_USER",
			"", "bob",
			func(t *testing.T, c *Config) {
				if c.Photos.User != "bob" || !legacyUsed(c, "OC_USER") {
					t.Errorf("got %q", c.Photos.User)
				}
			}},
		{"photos token legacy", "OCAPPS_PHOTOS_APP_TOKEN", "OC_APP_TOKEN",
			"", "legacy-token",
			func(t *testing.T, c *Config) {
				if c.Photos.AppToken != "legacy-token" || !legacyUsed(c, "OC_APP_TOKEN") {
					t.Errorf("got %q", c.Photos.AppToken)
				}
			}},
		{"memories_token legacy", "OCAPPS_PHOTOS_TOKEN", "MEMORIES_TOKEN",
			"", "mtok",
			func(t *testing.T, c *Config) {
				if c.Photos.Token != "mtok" || !legacyUsed(c, "MEMORIES_TOKEN") {
					t.Errorf("got %q", c.Photos.Token)
				}
			}},
		{"scan_root legacy", "OCAPPS_PHOTOS_SCAN_ROOT", "SCAN_ROOT",
			"", "Imágenes",
			func(t *testing.T, c *Config) {
				if c.Photos.ScanRoot != "Imágenes" || !legacyUsed(c, "SCAN_ROOT") {
					t.Errorf("got %q", c.Photos.ScanRoot)
				}
			}},
		{"scan_every legacy", "OCAPPS_PHOTOS_SCAN_EVERY", "SCAN_EVERY",
			"", "30m",
			func(t *testing.T, c *Config) {
				if c.Photos.ScanEvery != 30*time.Minute || !legacyUsed(c, "SCAN_EVERY") {
					t.Errorf("got %s", c.Photos.ScanEvery)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base(t)
			// la variable nueva del caso: con su valor, o DESDEFINIDA
			// (base define algunas) para que gane la legacy
			if tc.setNueva != "" {
				t.Setenv(tc.nueva, tc.setNueva)
			} else {
				unsetEnv(t, tc.nueva)
			}
			if tc.setLegacy != "" {
				t.Setenv(tc.legacy, tc.setLegacy)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tc.check(t, cfg)
		})
	}
}

// TestNuevaVaciaPrevaleceSobreLegacy (M9): una OCAPPS_* definida
// EXPLÍCITAMENTE vacía ignora la legacy (es la forma de anular una legacy
// heredada del entorno); una legacy vacía sigue contando como no definida.
func TestNuevaVaciaPrevaleceSobreLegacy(t *testing.T) {
	t.Run("string: nueva vacía anula la legacy", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_NEWS_NTFY_TOPIC", "")
		t.Setenv("OCNEWS_NTFY_TOPIC", "topic-legacy")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.News.NtfyTopic != "" {
			t.Fatalf("NtfyTopic: %q (la nueva vacía debe prevalecer sobre la legacy)", cfg.News.NtfyTopic)
		}
		if legacyUsed(cfg, "OCNEWS_NTFY_TOPIC") {
			t.Error("legacy registrada como usada con la nueva definida (vacía)")
		}
	})
	t.Run("duration: nueva vacía bloquea la legacy y cae al default", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_NEWS_FEED_INTERVAL", "")
		t.Setenv("OCNEWS_FEED_INTERVAL", "1h")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.News.FeedInterval != DefaultFeedInterval {
			t.Fatalf("FeedInterval: %s, want default %s", cfg.News.FeedInterval, DefaultFeedInterval)
		}
		if legacyUsed(cfg, "OCNEWS_FEED_INTERVAL") {
			t.Error("legacy registrada como usada con la nueva definida (vacía)")
		}
	})
	t.Run("legacy vacía = no definida (comportamiento conservado)", func(t *testing.T) {
		base(t)
		unsetEnv(t, "OCAPPS_NEWS_NTFY_TOPIC")
		t.Setenv("OCNEWS_NTFY_TOPIC", "")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.News.NtfyTopic != "" {
			t.Fatalf("NtfyTopic: %q (legacy vacía no debe contar)", cfg.News.NtfyTopic)
		}
		if legacyUsed(cfg, "OCNEWS_NTFY_TOPIC") {
			t.Error("legacy vacía registrada como usada")
		}
	})
}

// TestOCNotesGraphURLStrip: OCNOTES_GRAPH_URL es la URL completa .../graph/v1.0/me;
// al mapearla se deriva la raíz (§3.2 †).
func TestOCNotesGraphURLStrip(t *testing.T) {
	base(t)
	unsetEnv(t, "OCAPPS_OPENCLOUD_URL")
	t.Setenv("OCNOTES_GRAPH_URL", "https://notes.example.com/graph/v1.0/me")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OpenCloudURL != "https://notes.example.com" {
		t.Fatalf("raíz no derivada: %q", cfg.OpenCloudURL)
	}
	if !legacyUsed(cfg, "OCNOTES_GRAPH_URL") {
		t.Fatal("legacy no registrada")
	}

	// si no trae el sufijo, se usa tal cual
	base(t)
	unsetEnv(t, "OCAPPS_OPENCLOUD_URL")
	t.Setenv("OCNOTES_GRAPH_URL", "https://notes.example.com/")
	cfg2, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.OpenCloudURL != "https://notes.example.com" {
		t.Fatalf("got %q", cfg2.OpenCloudURL)
	}
}

// TestListenAddrLegacyNoHeredaPuerto: las legacy de addr solo loguean
// (§3.2); el puerto no se hereda.
func TestListenAddrLegacyNoHeredaPuerto(t *testing.T) {
	base(t)
	t.Setenv("OCNEWS_ADDR", ":8094")
	t.Setenv("OCNOTES_ADDR", ":8100")
	t.Setenv("LISTEN_ADDR", ":9210")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != DefaultListenAddr {
		t.Fatalf("puerto heredado indebidamente: %q", cfg.ListenAddr)
	}
	for _, legacy := range []string{"OCNEWS_ADDR", "OCNOTES_ADDR", "LISTEN_ADDR"} {
		if !legacyUsed(cfg, legacy) {
			t.Errorf("%s no registrada", legacy)
		}
	}

	// la nueva sí manda
	base(t)
	t.Setenv("OCAPPS_LISTEN_ADDR", "0.0.0.0:9000")
	cfg2, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.ListenAddr != "0.0.0.0:9000" {
		t.Fatalf("got %q", cfg2.ListenAddr)
	}
}

// TestDataDirLegacy: fallback común y overrides por módulo (transición suave).
func TestDataDirLegacy(t *testing.T) {
	t.Run("legacy por módulo define la ruta de ese módulo", func(t *testing.T) {
		base(t)
		legacyDir := t.TempDir() + "/ocnews"
		unsetEnv(t, "OCAPPS_DATA_DIR")
		t.Setenv("OCNEWS_DATA_DIR", legacyDir)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.DataDir != legacyDir { // primera legacy en precedencia
			t.Errorf("DataDir: %q", cfg.DataDir)
		}
		if cfg.NewsDataDir != legacyDir { // sin subdir: transición suave
			t.Errorf("NewsDataDir: %q", cfg.NewsDataDir)
		}
		if cfg.NotesDataDir != legacyDir+"/notes" {
			t.Errorf("NotesDataDir: %q", cfg.NotesDataDir)
		}
		if cfg.PhotosDataDir != legacyDir+"/photos" {
			t.Errorf("PhotosDataDir: %q", cfg.PhotosDataDir)
		}
	})
	t.Run("OCAPPS_DATA_DIR gana y cuelgan subdirs", func(t *testing.T) {
		base(t)
		t.Setenv("OCNEWS_DATA_DIR", "/var/lib/ocnews")
		dir := t.TempDir()
		t.Setenv("OCAPPS_DATA_DIR", dir)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.DataDir != dir {
			t.Errorf("DataDir: %q", cfg.DataDir)
		}
		// la legacy por módulo sigue mandando para news (§3.2)
		if cfg.NewsDataDir != "/var/lib/ocnews" {
			t.Errorf("NewsDataDir: %q", cfg.NewsDataDir)
		}
		if cfg.NotesDataDir != dir+"/notes" || cfg.PhotosDataDir != dir+"/photos" {
			t.Errorf("notes/photos: %q %q", cfg.NotesDataDir, cfg.PhotosDataDir)
		}
	})
}

// TestWebDirIgnorada: WEB_DIR eliminada (Q6) → warn "ignorada".
func TestWebDirIgnorada(t *testing.T) {
	base(t)
	t.Setenv("WEB_DIR", "/app/web")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !legacyUsed(cfg, "WEB_DIR") {
		t.Fatal("WEB_DIR no registrada")
	}
}

// TestValidacionFatal: §3.3 — solo lo común es fatal.
func TestValidacionFatal(t *testing.T) {
	t.Run("opencloud sin URL", func(t *testing.T) {
		base(t)
		unsetEnv(t, "OCAPPS_OPENCLOUD_URL")
		if _, err := Load(); err == nil {
			t.Fatal("esperaba error fatal")
		}
	})
	t.Run("modo local no exige URL", func(t *testing.T) {
		base(t)
		unsetEnv(t, "OCAPPS_OPENCLOUD_URL")
		t.Setenv("OCAPPS_AUTH_MODE", "local")
		if _, err := Load(); err != nil {
			t.Fatalf("Load: %v", err)
		}
	})
	t.Run("log level inválido", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_LOG_LEVEL", "trace")
		if _, err := Load(); err == nil {
			t.Fatal("esperaba error fatal")
		}
	})
	t.Run("auth mode inválido", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_AUTH_MODE", "oidc")
		if _, err := Load(); err == nil {
			t.Fatal("esperaba error fatal")
		}
	})
	t.Run("data dir no escribible", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_DATA_DIR", "/proc/ocapps-no-se-puede")
		if _, err := Load(); err == nil {
			t.Fatal("esperaba error fatal")
		}
	})
	t.Run("enabled inválido", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_NEWS_ENABLED", "tal vez")
		if _, err := Load(); err == nil {
			t.Fatal("esperaba error fatal")
		}
	})
}

// TestErroresDeModuloNoFatales: durations inválidas de news y credenciales de
// photos vacías degradan el módulo, no el proceso (§3.3).
func TestErroresDeModuloNoFatales(t *testing.T) {
	t.Run("duration inválida de news", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_NEWS_FETCH_TIMEOUT", "no-es-una-duracion")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load no debe ser fatal: %v", err)
		}
		merr := cfg.ModuleErr("news")
		if merr == nil || !strings.Contains(merr.Error(), "OCAPPS_NEWS_FETCH_TIMEOUT") {
			t.Fatalf("ModuleErr(news): %v", merr)
		}
		if cfg.News.FetchTimeout != DefaultFetchTimeout {
			t.Errorf("valor contaminado: %s", cfg.News.FetchTimeout)
		}
		if cfg.ModuleErr("photos") != nil || cfg.ModuleErr("notes") != nil {
			t.Error("otros módulos no deben degradarse")
		}
	})
	t.Run("duration legacy inválida nombra la legacy", func(t *testing.T) {
		base(t)
		t.Setenv("OCNEWS_FEED_INTERVAL", "xyz")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if merr := cfg.ModuleErr("news"); merr == nil || !strings.Contains(merr.Error(), "OCNEWS_FEED_INTERVAL") {
			t.Fatalf("ModuleErr(news): %v", merr)
		}
		if !legacyUsed(cfg, "OCNEWS_FEED_INTERVAL") {
			t.Error("legacy inválida no registrada")
		}
	})
	t.Run("max_gap < feed_interval", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_NEWS_FEED_INTERVAL", "1h")
		t.Setenv("OCAPPS_NEWS_MAX_GAP", "30m")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.ModuleErr("news") == nil {
			t.Fatal("esperaba ModuleErr(news)")
		}
	})
	t.Run("retention negativa", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_NEWS_RETENTION_DAYS", "-5")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.ModuleErr("news") == nil {
			t.Fatal("esperaba ModuleErr(news)")
		}
	})
	// H8: photos es multi-tenant; OCAPPS_PHOTOS_USER/APP_TOKEN ya no son
	// obligatorios y su ausencia NO degrada el módulo.
	t.Run("photos sin user/token es válido (H8)", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_PHOTOS_USER", "")
		t.Setenv("OCAPPS_PHOTOS_APP_TOKEN", "")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load no debe ser fatal: %v", err)
		}
		if cfg.ModuleErr("photos") != nil {
			t.Fatalf("sin user/token ya no degrada el módulo (H8): %v", cfg.ModuleErr("photos"))
		}
		if len(cfg.Photos.Users) != 0 {
			t.Errorf("Users sin nada configurado: %+v", cfg.Photos.Users)
		}
		if cfg.ModuleErr("news") != nil {
			t.Error("news no debe degradarse")
		}
	})
	// H8: el Bearer estático DEPRECATED sin USER no tiene identidad -> error.
	t.Run("photos token estático sin user (H8)", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_PHOTOS_USER", "")
		t.Setenv("OCAPPS_PHOTOS_APP_TOKEN", "")
		t.Setenv("OCAPPS_PHOTOS_TOKEN", "static-tok")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load no debe ser fatal: %v", err)
		}
		merr := cfg.ModuleErr("photos")
		if merr == nil || !strings.Contains(merr.Error(), "OCAPPS_PHOTOS_TOKEN") {
			t.Fatalf("ModuleErr(photos): %v", merr)
		}
		if cfg.ModuleErr("news") != nil {
			t.Error("news no debe degradarse")
		}
	})
	// H8: OCAPPS_PHOTOS_USERS malformado -> error del módulo (D3).
	t.Run("photos users malformado (H8)", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_PHOTOS_USERS", "alice:tok1,bob-sin-token")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load no debe ser fatal: %v", err)
		}
		merr := cfg.ModuleErr("photos")
		if merr == nil || !strings.Contains(merr.Error(), "OCAPPS_PHOTOS_USERS") {
			t.Fatalf("ModuleErr(photos): %v", merr)
		}
	})
	t.Run("photos disabled no exige credenciales", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_PHOTOS_USER", "")
		t.Setenv("OCAPPS_PHOTOS_APP_TOKEN", "")
		t.Setenv("OCAPPS_PHOTOS_ENABLED", "false")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.PhotosEnabled {
			t.Fatal("photos debe estar disabled")
		}
		if cfg.ModuleErr("photos") != nil {
			t.Fatalf("ModuleErr(photos): %v", cfg.ModuleErr("photos"))
		}
	})
	t.Run("scan_every inválido", func(t *testing.T) {
		base(t)
		t.Setenv("SCAN_EVERY", "-5m")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.ModuleErr("photos") == nil {
			t.Fatal("esperaba ModuleErr(photos)")
		}
	})
}

// TestPhotosUsers: parseo de OCAPPS_PHOTOS_USERS y pliegue del par legacy
// USER+APP_TOKEN en la misma lista (H8 §6.2).
func TestPhotosUsers(t *testing.T) {
	t.Run("varios usuarios", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_PHOTOS_USER", "")
		t.Setenv("OCAPPS_PHOTOS_APP_TOKEN", "")
		t.Setenv("OCAPPS_PHOTOS_USERS", "alice:apptoken1, bob:apptoken2")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.ModuleErr("photos") != nil {
			t.Fatalf("ModuleErr: %v", cfg.ModuleErr("photos"))
		}
		want := []AppUser{{User: "alice", Token: "apptoken1"}, {User: "bob", Token: "apptoken2"}}
		if len(cfg.Photos.Users) != 2 || cfg.Photos.Users[0] != want[0] || cfg.Photos.Users[1] != want[1] {
			t.Fatalf("Users: %+v", cfg.Photos.Users)
		}
	})
	t.Run("pliegue legacy", func(t *testing.T) {
		base(t) // define OCAPPS_PHOTOS_USER=alice + APP_TOKEN=tok
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(cfg.Photos.Users) != 1 || cfg.Photos.Users[0] != (AppUser{User: "alice", Token: "tok"}) {
			t.Fatalf("pliegue legacy: %+v", cfg.Photos.Users)
		}
	})
	t.Run("legacy ya listado no se duplica", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_PHOTOS_USERS", "alice:apptoken1,bob:apptoken2")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(cfg.Photos.Users) != 2 {
			t.Fatalf("alice ya estaba listada: %+v", cfg.Photos.Users)
		}
	})
	t.Run("entradas malformadas", func(t *testing.T) {
		for _, raw := range []string{"alice", ":tok", "alice:", "alice:tok,bob"} {
			if _, err := parsePhotosUsers(raw); err == nil {
				t.Errorf("parsePhotosUsers(%q) debería fallar", raw)
			}
		}
		if got, err := parsePhotosUsers(""); err != nil || got != nil {
			t.Errorf("vacío: %v %v", got, err)
		}
	})
}
