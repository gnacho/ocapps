package extensions

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Status values reported to the panel.
const (
	StatusUpToDate = "up_to_date"
	StatusUpdate   = "update_available"
	StatusLocal    = "local_build"
	StatusError    = "error"
)

// App is one row of the panel.
type App struct {
	ID        string `json:"id"`
	Repo      string `json:"repo"`
	Installed string `json:"installed"`
	Latest    string `json:"latest"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
}

// RunResult is the outcome of the last helper invocation, kept so the panel
// can show what happened after the page reloads (OpenCloud restarts during an
// update, so the response may never reach the browser).
type RunResult struct {
	App      string    `json:"app"`
	OK       bool      `json:"ok"`
	Output   string    `json:"output"`
	Finished time.Time `json:"finished"`
}

// Service lists installed extensions and drives the ocapps-ext helper.
type Service struct {
	AppsDir string
	Helper  string
	UseSudo bool
	Timeout time.Duration
	Conf    []AppConf
	GitHub  *GitHub
	mu      sync.Mutex
	last    *RunResult
	running string
}

// NewService builds the service from the parsed configuration.
func NewService(appsDir, helper string, useSudo bool, conf []AppConf, gh *GitHub) *Service {
	return &Service{AppsDir: appsDir, Helper: helper, UseSudo: useSudo, Timeout: 5 * time.Minute, Conf: conf, GitHub: gh}
}

// Stamp returns the .ocapps-release content of an installed app, or "" when
// the app is not installed.
func (s *Service) Stamp(id string) string {
	b, err := os.ReadFile(filepath.Join(s.AppsDir, id, ".ocapps-release"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (s *Service) conf(id string) (AppConf, bool) {
	for _, c := range s.Conf {
		if c.ID == id {
			return c, true
		}
	}
	return AppConf{}, false
}

// List reports every configured app with its installed stamp and latest
// release. A failing GitHub lookup only marks that row, it does not fail the
// whole list.
func (s *Service) List(ctx context.Context) []App {
	out := make([]App, 0, len(s.Conf))
	for _, c := range s.Conf {
		app := App{ID: c.ID, Repo: c.Repo, Installed: s.Stamp(c.ID)}
		rel, err := s.GitHub.Latest(ctx, c.Repo, c.Regex)
		switch {
		case err != nil:
			app.Status = StatusError
			app.Error = err.Error()
		case app.Installed == "":
			app.Status = StatusUpdate
			app.Latest = rel.Tag
		case app.Installed == "release:"+rel.Tag:
			app.Status = StatusUpToDate
			app.Latest = rel.Tag
		case strings.HasPrefix(app.Installed, "local:"):
			app.Status = StatusLocal
			app.Latest = rel.Tag
		default:
			app.Status = StatusUpdate
			app.Latest = rel.Tag
		}
		out = append(out, app)
	}
	return out
}

// Update runs `ocapps-ext update <id>` through sudo. Only ids present in the
// configuration are accepted, so the HTTP surface cannot be used to run the
// helper with arbitrary arguments. force replaces a local build too.
func (s *Service) Update(ctx context.Context, id string, force bool) (RunResult, error) {
	if _, ok := s.conf(id); !ok {
		return RunResult{}, fmt.Errorf("unknown app %q", id)
	}

	s.mu.Lock()
	if s.running != "" {
		running := s.running
		s.mu.Unlock()
		return RunResult{}, fmt.Errorf("an update is already running (%s)", running)
	}
	s.running = id
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.running = ""
		s.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()

	args := []string{"update"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, id)
	var cmd *exec.Cmd
	if s.UseSudo {
		cmd = exec.CommandContext(ctx, "sudo", append([]string{"-n", s.Helper}, args...)...)
	} else {
		cmd = exec.CommandContext(ctx, s.Helper, args...)
	}
	cmd.Env = append(os.Environ(), "OCAPPS_EXT_IN_PROGRESS=1")

	out, err := cmd.CombinedOutput()
	res := RunResult{App: id, OK: err == nil, Output: string(out), Finished: time.Now()}

	s.mu.Lock()
	s.last = &res
	s.mu.Unlock()

	if err != nil {
		return res, fmt.Errorf("ocapps-ext: %w", err)
	}
	return res, nil
}

// Last returns the outcome of the most recent helper run, if any.
func (s *Service) Last() *RunResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		return nil
	}
	cp := *s.last
	return &cp
}

// Running returns the id of the app being updated, if any.
func (s *Service) Running() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}
