package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// allVars: every variable of the §3.2 schema (new and legacy). Each test
// UNSETS them to isolate the case (M9: an OCAPPS_* defined as empty no longer
// counts as not defined, so blanking with Setenv(k, "") does not work).
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

// unsetEnv UNSETS every variable and restores its previous value when the
// test ends (equivalent to t.Setenv but for "not defined").
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

// base prepares a clean and valid environment (opencloud auth + own data dir).
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

func TestEmptyScanRootFallsBackToDefault(t *testing.T) {
	base(t)
	// A blank line in the env must not become "walk the whole space"
	// (issue #36): empty = not configured.
	t.Setenv("OCAPPS_PHOTOS_SCAN_ROOT", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Photos.ScanRoot != DefaultScanRoot {
		t.Errorf("empty ScanRoot: got %q, want %q", cfg.Photos.ScanRoot, DefaultScanRoot)
	}

	t.Setenv("OCAPPS_PHOTOS_SCAN_ROOT", "/")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Photos.ScanRoot != "/" {
		t.Errorf("explicit ScanRoot: got %q, want %q", cfg.Photos.ScanRoot, "/")
	}
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
		t.Error("default enabled must be true x3")
	}
	// subdirs per module (§5.1)
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
		t.Errorf("no legacy expected: %+v", cfg.Legacy)
	}
}

// TestNewVarPrecedenceOverLegacy: for every §3.2 row with a legacy one, the
// OCAPPS_* value wins over the legacy one, the legacy one wins over the
// default, and using a legacy one is recorded (warning §3.1).
func TestNewVarPrecedenceOverLegacy(t *testing.T) {
	cases := []struct {
		name      string
		nueva     string
		legacy    string
		setNueva  string // "" = the new one is not defined
		setLegacy string
		check     func(t *testing.T, cfg *Config)
	}{
		{"new opencloud_url wins", "OCAPPS_OPENCLOUD_URL", "OCNEWS_OPENCOLOUD_URL",
			"https://nueva.example.com", "https://legacy.example.com",
			func(t *testing.T, c *Config) {
				if c.OpenCloudURL != "https://nueva.example.com" {
					t.Errorf("got %q", c.OpenCloudURL)
				}
				if legacyUsed(c, "OCNEWS_OPENCOLOUD_URL") {
					t.Error("legacy recorded without being used")
				}
			}},
		{"opencloud_url legacy typo", "OCAPPS_OPENCLOUD_URL", "OCNEWS_OPENCOLOUD_URL",
			"", "https://legacy.example.com/",
			func(t *testing.T, c *Config) {
				if c.OpenCloudURL != "https://legacy.example.com" { // trailing / trimmed
					t.Errorf("got %q", c.OpenCloudURL)
				}
				if !legacyUsed(c, "OCNEWS_OPENCOLOUD_URL") {
					t.Error("legacy typo not recorded")
				}
			}},
		{"oc_base_url legacy", "OCAPPS_OPENCLOUD_URL", "OC_BASE_URL",
			"", "https://photos-legacy.example.com",
			func(t *testing.T, c *Config) {
				if c.OpenCloudURL != "https://photos-legacy.example.com" {
					t.Errorf("got %q", c.OpenCloudURL)
				}
				if !legacyUsed(c, "OC_BASE_URL") {
					t.Error("OC_BASE_URL not recorded")
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
		{"retention 0 disables", "OCAPPS_NEWS_RETENTION_DAYS", "OCNEWS_RETENTION_DAYS",
			"0", "30",
			func(t *testing.T, c *Config) {
				if c.News.RetentionDays != 0 || c.News.Retention() != 0 {
					t.Errorf("retention not disabled: %+v", c.News)
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
			"", "my-topic",
			func(t *testing.T, c *Config) {
				if c.News.NtfyTopic != "my-topic" || !legacyUsed(c, "OCNEWS_NTFY_TOPIC") {
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
		{"generic auth_user legacy", "OCAPPS_NEWS_AUTH_USER", "AUTH_USER",
			"", "admin",
			func(t *testing.T, c *Config) {
				if c.News.AuthUser != "admin" || !legacyUsed(c, "AUTH_USER") {
					t.Errorf("got %q", c.News.AuthUser)
				}
			}},
		{"generic auth_pass legacy", "OCAPPS_NEWS_AUTH_PASS", "AUTH_PASS",
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
			"", "Images",
			func(t *testing.T, c *Config) {
				if c.Photos.ScanRoot != "Images" || !legacyUsed(c, "SCAN_ROOT") {
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
			// the new variable of the case: with its value, or UNSET
			// (base defines some) so that the legacy one wins
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

// TestEmptyNewVarWinsOverLegacy (M9): an OCAPPS_* explicitly defined as
// empty ignores the legacy one (it is the way to cancel a legacy one inherited
// from the environment), an empty legacy one still counts as not defined.
func TestEmptyNewVarWinsOverLegacy(t *testing.T) {
	t.Run("string: empty new one cancels the legacy one", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_NEWS_NTFY_TOPIC", "")
		t.Setenv("OCNEWS_NTFY_TOPIC", "topic-legacy")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.News.NtfyTopic != "" {
			t.Fatalf("NtfyTopic: %q (the empty new one must prevail over the legacy one)", cfg.News.NtfyTopic)
		}
		if legacyUsed(cfg, "OCNEWS_NTFY_TOPIC") {
			t.Error("legacy recorded as used with the new one defined (empty)")
		}
	})
	t.Run("duration: empty new one blocks the legacy one and falls back to the default", func(t *testing.T) {
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
			t.Error("legacy recorded as used with the new one defined (empty)")
		}
	})
	t.Run("empty legacy = not defined (behavior kept)", func(t *testing.T) {
		base(t)
		unsetEnv(t, "OCAPPS_NEWS_NTFY_TOPIC")
		t.Setenv("OCNEWS_NTFY_TOPIC", "")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.News.NtfyTopic != "" {
			t.Fatalf("NtfyTopic: %q (an empty legacy must not count)", cfg.News.NtfyTopic)
		}
		if legacyUsed(cfg, "OCNEWS_NTFY_TOPIC") {
			t.Error("empty legacy recorded as used")
		}
	})
}

// TestOCNotesGraphURLStrip: OCNOTES_GRAPH_URL is the full URL .../graph/v1.0/me,
// when mapping it the root is derived (§3.2 †).
func TestOCNotesGraphURLStrip(t *testing.T) {
	base(t)
	unsetEnv(t, "OCAPPS_OPENCLOUD_URL")
	t.Setenv("OCNOTES_GRAPH_URL", "https://notes.example.com/graph/v1.0/me")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OpenCloudURL != "https://notes.example.com" {
		t.Fatalf("root not derived: %q", cfg.OpenCloudURL)
	}
	if !legacyUsed(cfg, "OCNOTES_GRAPH_URL") {
		t.Fatal("legacy not recorded")
	}

	// if it does not carry the suffix, it is used as is
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

// TestListenAddrLegacyNoHeredaPuerto: the addr legacy ones only log (§3.2),
// the port is not inherited.
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
		t.Fatalf("port inherited improperly: %q", cfg.ListenAddr)
	}
	for _, legacy := range []string{"OCNEWS_ADDR", "OCNOTES_ADDR", "LISTEN_ADDR"} {
		if !legacyUsed(cfg, legacy) {
			t.Errorf("%s not recorded", legacy)
		}
	}

	// the new one does take precedence
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

// TestDataDirLegacy: common fallback and per module overrides (smooth transition).
func TestDataDirLegacy(t *testing.T) {
	t.Run("per module legacy sets the path of that module", func(t *testing.T) {
		base(t)
		legacyDir := t.TempDir() + "/ocnews"
		unsetEnv(t, "OCAPPS_DATA_DIR")
		t.Setenv("OCNEWS_DATA_DIR", legacyDir)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.DataDir != legacyDir { // first legacy in precedence
			t.Errorf("DataDir: %q", cfg.DataDir)
		}
		if cfg.NewsDataDir != legacyDir { // no subdir: smooth transition
			t.Errorf("NewsDataDir: %q", cfg.NewsDataDir)
		}
		if cfg.NotesDataDir != legacyDir+"/notes" {
			t.Errorf("NotesDataDir: %q", cfg.NotesDataDir)
		}
		if cfg.PhotosDataDir != legacyDir+"/photos" {
			t.Errorf("PhotosDataDir: %q", cfg.PhotosDataDir)
		}
	})
	t.Run("OCAPPS_DATA_DIR wins and subdirs hang from it", func(t *testing.T) {
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
		// the per module legacy one still wins for news (§3.2)
		if cfg.NewsDataDir != "/var/lib/ocnews" {
			t.Errorf("NewsDataDir: %q", cfg.NewsDataDir)
		}
		if cfg.NotesDataDir != dir+"/notes" || cfg.PhotosDataDir != dir+"/photos" {
			t.Errorf("notes/photos: %q %q", cfg.NotesDataDir, cfg.PhotosDataDir)
		}
	})
}

// TestWebDirIgnorada: WEB_DIR removed (Q6) -> "ignored" warning.
func TestWebDirIgnorada(t *testing.T) {
	base(t)
	t.Setenv("WEB_DIR", "/app/web")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !legacyUsed(cfg, "WEB_DIR") {
		t.Fatal("WEB_DIR not recorded")
	}
}

// TestValidacionFatal: §3.3 - only the common part is fatal.
func TestValidacionFatal(t *testing.T) {
	t.Run("opencloud without URL", func(t *testing.T) {
		base(t)
		unsetEnv(t, "OCAPPS_OPENCLOUD_URL")
		if _, err := Load(); err == nil {
			t.Fatal("expected a fatal error")
		}
	})
	t.Run("local mode does not require a URL", func(t *testing.T) {
		base(t)
		unsetEnv(t, "OCAPPS_OPENCLOUD_URL")
		t.Setenv("OCAPPS_AUTH_MODE", "local")
		if _, err := Load(); err != nil {
			t.Fatalf("Load: %v", err)
		}
	})
	t.Run("invalid log level", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_LOG_LEVEL", "trace")
		if _, err := Load(); err == nil {
			t.Fatal("expected a fatal error")
		}
	})
	t.Run("invalid auth mode", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_AUTH_MODE", "oidc")
		if _, err := Load(); err == nil {
			t.Fatal("expected a fatal error")
		}
	})
	t.Run("data dir not writable", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_DATA_DIR", "/proc/ocapps-no-se-puede")
		if _, err := Load(); err == nil {
			t.Fatal("expected a fatal error")
		}
	})
	t.Run("invalid enabled", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_NEWS_ENABLED", "maybe")
		if _, err := Load(); err == nil {
			t.Fatal("expected a fatal error")
		}
	})
}

// TestModuleErrorsAreNotFatal: invalid news durations and empty photos
// credentials degrade the module, not the process (§3.3).
func TestModuleErrorsAreNotFatal(t *testing.T) {
	t.Run("invalid news duration", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_NEWS_FETCH_TIMEOUT", "not-a-duration")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load must not be fatal: %v", err)
		}
		merr := cfg.ModuleErr("news")
		if merr == nil || !strings.Contains(merr.Error(), "OCAPPS_NEWS_FETCH_TIMEOUT") {
			t.Fatalf("ModuleErr(news): %v", merr)
		}
		if cfg.News.FetchTimeout != DefaultFetchTimeout {
			t.Errorf("contaminated value: %s", cfg.News.FetchTimeout)
		}
		if cfg.ModuleErr("photos") != nil || cfg.ModuleErr("notes") != nil {
			t.Error("other modules must not degrade")
		}
	})
	t.Run("invalid legacy duration names the legacy one", func(t *testing.T) {
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
			t.Error("invalid legacy not recorded")
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
			t.Fatal("expected ModuleErr(news)")
		}
	})
	t.Run("negative retention", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_NEWS_RETENTION_DAYS", "-5")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.ModuleErr("news") == nil {
			t.Fatal("expected ModuleErr(news)")
		}
	})
	// H8: photos is multi-tenant, OCAPPS_PHOTOS_USER/APP_TOKEN are no longer
	// mandatory and their absence does NOT degrade the module.
	t.Run("photos without user/token is valid (H8)", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_PHOTOS_USER", "")
		t.Setenv("OCAPPS_PHOTOS_APP_TOKEN", "")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load must not be fatal: %v", err)
		}
		if cfg.ModuleErr("photos") != nil {
			t.Fatalf("without user/token it no longer degrades the module (H8): %v", cfg.ModuleErr("photos"))
		}
		if len(cfg.Photos.Users) != 0 {
			t.Errorf("Users with nothing configured: %+v", cfg.Photos.Users)
		}
		if cfg.ModuleErr("news") != nil {
			t.Error("news must not degrade")
		}
	})
	// H8: the DEPRECATED static Bearer without USER has no identity -> error.
	t.Run("static photos token without user (H8)", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_PHOTOS_USER", "")
		t.Setenv("OCAPPS_PHOTOS_APP_TOKEN", "")
		t.Setenv("OCAPPS_PHOTOS_TOKEN", "static-tok")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load must not be fatal: %v", err)
		}
		merr := cfg.ModuleErr("photos")
		if merr == nil || !strings.Contains(merr.Error(), "OCAPPS_PHOTOS_TOKEN") {
			t.Fatalf("ModuleErr(photos): %v", merr)
		}
		if cfg.ModuleErr("news") != nil {
			t.Error("news must not degrade")
		}
	})
	// H8: malformed OCAPPS_PHOTOS_USERS -> module error (D3).
	t.Run("malformed photos users (H8)", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_PHOTOS_USERS", "alice:tok1,bob-sin-token")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load must not be fatal: %v", err)
		}
		merr := cfg.ModuleErr("photos")
		if merr == nil || !strings.Contains(merr.Error(), "OCAPPS_PHOTOS_USERS") {
			t.Fatalf("ModuleErr(photos): %v", merr)
		}
	})
	t.Run("photos disabled does not require credentials", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_PHOTOS_USER", "")
		t.Setenv("OCAPPS_PHOTOS_APP_TOKEN", "")
		t.Setenv("OCAPPS_PHOTOS_ENABLED", "false")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.PhotosEnabled {
			t.Fatal("photos must be disabled")
		}
		if cfg.ModuleErr("photos") != nil {
			t.Fatalf("ModuleErr(photos): %v", cfg.ModuleErr("photos"))
		}
	})
	t.Run("invalid scan_every", func(t *testing.T) {
		base(t)
		t.Setenv("SCAN_EVERY", "-5m")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.ModuleErr("photos") == nil {
			t.Fatal("expected ModuleErr(photos)")
		}
	})
}

// TestPhotosUsers: parsing of OCAPPS_PHOTOS_USERS and folding of the legacy
// USER+APP_TOKEN pair into the same list (H8 §6.2).
func TestPhotosUsers(t *testing.T) {
	t.Run("several users", func(t *testing.T) {
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
	t.Run("legacy folding", func(t *testing.T) {
		base(t) // defines OCAPPS_PHOTOS_USER=alice + APP_TOKEN=tok
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(cfg.Photos.Users) != 1 || cfg.Photos.Users[0] != (AppUser{User: "alice", Token: "tok"}) {
			t.Fatalf("legacy folding: %+v", cfg.Photos.Users)
		}
	})
	t.Run("legacy already listed is not duplicated", func(t *testing.T) {
		base(t)
		t.Setenv("OCAPPS_PHOTOS_USERS", "alice:apptoken1,bob:apptoken2")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(cfg.Photos.Users) != 2 {
			t.Fatalf("alice was already listed: %+v", cfg.Photos.Users)
		}
	})
	t.Run("malformed entries", func(t *testing.T) {
		for _, raw := range []string{"alice", ":tok", "alice:", "alice:tok,bob"} {
			if _, err := parsePhotosUsers(raw); err == nil {
				t.Errorf("parsePhotosUsers(%q) should fail", raw)
			}
		}
		if got, err := parsePhotosUsers(""); err != nil || got != nil {
			t.Errorf("empty: %v %v", got, err)
		}
	})
}
