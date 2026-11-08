// Package extensions exposes the web-extension installer (ocapps-ext) over
// HTTP so it can be driven from a panel inside OpenCloud.
//
// The service itself runs as an unprivileged user: installing extensions and
// restarting OpenCloud needs root, so every action is delegated to the
// ocapps-ext helper through a narrow sudoers rule. The HTTP surface is
// disabled when no admin token is configured.
package extensions

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// AppConf is one line of /etc/ocapps/extensions.conf:
//
//	<app-id>|<github repo>|<asset name regex>
type AppConf struct {
	ID    string
	Repo  string
	Regex *regexp.Regexp
}

// LoadConf parses the updater configuration. Blank lines and comments are
// ignored, and a malformed line is an error instead of being skipped: a typo
// here would silently stop an app from being updated.
func LoadConf(path string) ([]AppConf, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []AppConf
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) != 3 {
			return nil, fmt.Errorf("%s:%d: expected app|repo|regex", path, n)
		}
		id, repo, pattern := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2])
		if id == "" || repo == "" || pattern == "" {
			return nil, fmt.Errorf("%s:%d: empty field", path, n)
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		out = append(out, AppConf{ID: id, Repo: repo, Regex: re})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no apps configured", path)
	}
	return out, nil
}
