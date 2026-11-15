//go:build ignore

// Command thirdparty regenerates THIRD_PARTY.md from the modules the build and
// the tests actually compile.
//
// It is a mechanical generator: it collects the modules behind the packages of
// ./..., reads their license files and prints one markdown table row per
// module. The wording of the document is written by hand and lives in the
// template below.
//
//	go run tools/thirdparty/main.go > THIRD_PARTY.md
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type module struct {
	Path    string
	Version string
	Main    bool
	Dir     string
	Replace *struct {
		Path    string
		Version string
		Dir     string
	}
}

// licenseFiles are the file names that may carry the license text.
var licenseFiles = []string{
	"LICENSE", "LICENSE.md", "LICENSE.txt", "LICENCE", "LICENCE.md", "LICENCE.txt",
	"COPYING", "COPYING.md", "COPYING.txt", "UNLICENSE", "LICENSE-MIT", "LICENSE-APACHE",
}

// markers maps a normalized signature to an SPDX identifier. The text is
// lowercased and its whitespace collapsed before matching, and the first match
// wins. The order matters: short licenses go first and the GNU family last,
// because the MPL text quotes the GPL and the AGPL in its exhibits. The
// signatures are deliberately narrow: a wrong guess is worse than an explicit
// "unrecognized".
var markers = []struct {
	needle string
	spdx   string
}{
	{"mozilla public license", "MPL-2.0"},
	{"apache license", "Apache-2.0"},
	{"creative commons attribution 4.0", "CC-BY-4.0"},
	{"this is free and unencumbered software released into the public domain", "Unlicense"},
	{"permission is hereby granted, free of charge", "MIT"},
	{"permission to use, copy, modify, and/or distribute", "ISC"},
	{"redistribution and use in source and binary forms", "BSD"},
	{"gnu affero general public license", "AGPL"},
	{"gnu lesser general public license", "LGPL"},
	{"gnu library general public license", "LGPL-2.0"},
	{"gnu general public license", "GPL"},
	{"everyone is permitted to copy and distribute verbatim copies", "GPL"},
}

var spaces = regexp.MustCompile(`\s+`)

func main() {
	paths, err := compiledModules()
	if err != nil {
		fail(err)
	}
	rows := make([]string, 0, len(paths))
	for _, path := range paths {
		m, err := moduleInfo(path)
		if err != nil {
			fail(err)
		}
		if m.Main {
			continue
		}
		dir := m.Dir
		version := m.Version
		if m.Replace != nil {
			if m.Replace.Dir != "" {
				dir = m.Replace.Dir
			}
			if m.Replace.Version != "" {
				version = m.Replace.Version
			}
		}
		if version == "" {
			version = "replaced"
		}
		rows = append(rows, fmt.Sprintf("| `%s` | %s | %s |", m.Path, version, detect(dir)))
	}
	sort.Strings(rows)

	var b bytes.Buffer
	b.WriteString(header)
	b.WriteString("| Module | Version | License |\n|---|---|---|\n")
	for _, r := range rows {
		b.WriteString(r)
		b.WriteString("\n")
	}
	b.WriteString(footer)
	os.Stdout.Write(b.Bytes())
}

const header = `# Third-party software

` + "`ocapps`" + ` is distributed under the AGPL-3.0-only license (see [` + "`LICENSE`" + `](LICENSE)).

This inventory lists the modules the build and the tests actually compile, and
the license each upstream declares. It is generated, do not edit it by hand:

` + "```sh\ngo run tools/thirdparty/main.go > THIRD_PARTY.md\n```" + `

CI regenerates it and fails if the committed file is out of date, so a new or
upgraded dependency cannot land without being listed here.

## Vendored code

` + "`third_party/goexif`" + ` is a modified copy of
[` + "`rwcarlsen/goexif`" + `](https://github.com/rwcarlsen/goexif), used through a
` + "`replace`" + ` directive. Upstream is BSD-2-Clause, and its license text is kept
next to the code in [` + "`third_party/goexif/LICENSE`" + `](third_party/goexif/LICENSE).

No other dependency is vendored: everything else is fetched from the Go module
proxy at build time.

## Modules

`

const footer = `
The license column is detected from the license file the module ships in the
module cache. ` + "`unrecognized`" + ` means no license file was found, or its text did not
match a known signature: check upstream before relying on that module.
`

// compiledModules returns the sorted, de-duplicated module paths behind the
// packages of ./..., including their test dependencies.
func compiledModules() ([]string, error) {
	out, err := exec.Command("go", "list", "-deps", "-test",
		"-f", "{{if .Module}}{{.Module.Path}}{{end}}", "./...").Output()
	if err != nil {
		return nil, fmt.Errorf("go list -deps: %w", err)
	}
	seen := map[string]bool{}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		paths = append(paths, line)
	}
	sort.Strings(paths)
	return paths, nil
}

// moduleInfo returns the module metadata of one module path.
func moduleInfo(path string) (module, error) {
	out, err := exec.Command("go", "list", "-m", "-json", path).Output()
	if err != nil {
		return module{}, fmt.Errorf("go list -m %s: %w", path, err)
	}
	var m module
	if err := json.Unmarshal(out, &m); err != nil {
		return module{}, fmt.Errorf("decode module %s: %w", path, err)
	}
	return m, nil
}

// detect reads the license files of a module directory and returns the
// detected identifiers, joined when a module ships more than one.
func detect(dir string) string {
	if dir == "" {
		return "unrecognized"
	}
	seen := map[string]bool{}
	var found []string
	for _, name := range licenseFiles {
		text, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		spdx := classify(string(text))
		if spdx == "" || seen[spdx] {
			continue
		}
		seen[spdx] = true
		found = append(found, spdx)
	}
	if len(found) == 0 {
		return "unrecognized"
	}
	sort.Strings(found)
	return strings.Join(found, ", ")
}

// classify returns the SPDX identifier of a license text, or an empty string.
func classify(text string) string {
	normalized := spaces.ReplaceAllString(strings.ToLower(text), " ")
	for _, m := range markers {
		if !strings.Contains(normalized, m.needle) {
			continue
		}
		switch m.spdx {
		case "BSD":
			return bsdVariant(normalized)
		case "GPL", "LGPL", "AGPL":
			return gnuVariant(normalized, m.spdx)
		default:
			return m.spdx
		}
	}
	return ""
}

func bsdVariant(text string) string {
	switch {
	case strings.Contains(text, "neither the name") && strings.Contains(text, "endorse or promote"):
		return "BSD-3-Clause"
	case strings.Contains(text, "redistributions in binary form"):
		return "BSD-2-Clause"
	default:
		return "BSD"
	}
}

func gnuVariant(text, family string) string {
	version := ""
	switch {
	case strings.Contains(text, "version 3"):
		version = "3.0"
	case strings.Contains(text, "version 2.1"):
		version = "2.1"
	case strings.Contains(text, "version 2"):
		version = "2.0"
	}
	switch family {
	case "AGPL":
		if version == "3.0" {
			return "AGPL-3.0"
		}
		return "AGPL"
	case "LGPL":
		if version != "" {
			return "LGPL-" + version
		}
		return "LGPL"
	default:
		if version != "" {
			return "GPL-" + version
		}
		return "GPL"
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "thirdparty:", err)
	os.Exit(1)
}
