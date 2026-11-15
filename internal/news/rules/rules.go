// Package rules: advanced block/keep regex filters in the Miniflux
// style. Each line has the Field=regex format (RE2). Supported fields:
// EntryTitle, EntryURL, EntryAuthor, EntryContent and EntryDate (with date
// patterns: future, before:, after:, between:, max-age:).
//
// Evaluation (Miniflux order, first match wins):
//  1. Block rules: if any matches -> the article is ignored.
//  2. Keep rules: if there is any and none matches -> the article is ignored.
//
// Global and per feed are composed separately: global block first, then
// feed block, then global keep, then feed keep.
package rules

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

type Field string

const (
	FieldTitle   Field = "EntryTitle"
	FieldURL     Field = "EntryURL"
	FieldAuthor  Field = "EntryAuthor"
	FieldContent Field = "EntryContent"
	FieldDate    Field = "EntryDate"
)

var knownFields = map[Field]bool{
	FieldTitle: true, FieldURL: true, FieldAuthor: true, FieldContent: true, FieldDate: true,
}

// Rule is a compiled rule.
type Rule struct {
	field Field
	re    *regexp.Regexp
	date  datePat
}

type datePat struct {
	kind   string // future|before|after|between|maxage
	t0     int64
	t1     int64
	maxAge time.Duration
}

// RuleSet groups the compiled block and keep rules.
type RuleSet struct {
	block []Rule
	keep  []Rule
}

// Fields are the evaluable fields of an article.
type Fields struct {
	Title   string
	URL     string
	Author  string
	Content string // plain text of the body
	PubDate int64
}

// Parse compiles the rules of a multiline text. It returns an error with the
// problematic line if any rule is invalid.
func Parse(block, keep string) (*RuleSet, error) {
	b, err := parseLines(block)
	if err != nil {
		return nil, err
	}
	k, err := parseLines(keep)
	if err != nil {
		return nil, err
	}
	return &RuleSet{block: b, keep: k}, nil
}

func parseLines(text string) ([]Rule, error) {
	var out []Rule
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 {
			return nil, fmt.Errorf("rule without 'Field=regex': %q", line)
		}
		field := Field(strings.TrimSpace(line[:eq]))
		if !knownFields[field] {
			return nil, fmt.Errorf("unknown field %q in %q", field, line)
		}
		pat := strings.TrimSpace(line[eq+1:])
		if pat == "" {
			return nil, fmt.Errorf("empty regex in %q", line)
		}
		if field == FieldDate {
			d, err := parseDatePat(pat)
			if err != nil {
				return nil, err
			}
			out = append(out, Rule{field: field, date: d})
			continue
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, fmt.Errorf("invalid regex in %q: %w", line, err)
		}
		out = append(out, Rule{field: field, re: re})
	}
	return out, nil
}

// Ignore decides whether the article must be discarded according to THIS set of
// rules (block/keep of the same level).
func (rs *RuleSet) Ignore(f Fields) bool {
	if rs == nil {
		return false
	}
	for _, r := range rs.block {
		if r.matches(f) {
			return true
		}
	}
	if len(rs.keep) > 0 {
		for _, r := range rs.keep {
			if r.matches(f) {
				return false
			}
		}
		return true
	}
	return false
}

func (r Rule) matches(f Fields) bool {
	switch r.field {
	case FieldTitle:
		return r.re.MatchString(f.Title)
	case FieldURL:
		return r.re.MatchString(f.URL)
	case FieldAuthor:
		return r.re.MatchString(f.Author)
	case FieldContent:
		return r.re.MatchString(f.Content)
	case FieldDate:
		return r.dateMatches(f.PubDate)
	}
	return false
}

func (r Rule) dateMatches(pub int64) bool {
	switch r.date.kind {
	case "future":
		return pub > time.Now().Unix()
	case "before":
		return pub < r.date.t0
	case "after":
		return pub > r.date.t0
	case "between":
		return pub >= r.date.t0 && pub <= r.date.t1
	case "maxage":
		return pub < time.Now().Add(-r.date.maxAge).Unix()
	}
	return false
}

func parseDatePat(pat string) (datePat, error) {
	p := strings.ToLower(strings.TrimSpace(pat))
	switch {
	case p == "future":
		return datePat{kind: "future"}, nil
	case strings.HasPrefix(p, "before:"):
		t, err := parseDate(strings.TrimSpace(p[len("before:"):]))
		return datePat{kind: "before", t0: t}, err
	case strings.HasPrefix(p, "after:"):
		t, err := parseDate(strings.TrimSpace(p[len("after:"):]))
		return datePat{kind: "after", t0: t}, err
	case strings.HasPrefix(p, "between:"):
		parts := strings.Split(strings.TrimSpace(p[len("between:"):]), ",")
		if len(parts) != 2 {
			return datePat{}, fmt.Errorf("between requires two dates: %q", pat)
		}
		t0, err := parseDate(strings.TrimSpace(parts[0]))
		if err != nil {
			return datePat{}, err
		}
		t1, err := parseDate(strings.TrimSpace(parts[1]))
		if err != nil {
			return datePat{}, err
		}
		return datePat{kind: "between", t0: t0, t1: t1}, nil
	case strings.HasPrefix(p, "max-age:"):
		d, err := parseDuration(strings.TrimSpace(p[len("max-age:"):]))
		if err != nil || d <= 0 {
			return datePat{}, fmt.Errorf("invalid max-age %q (ns/µs/ms/s/m/h/d)", pat)
		}
		return datePat{kind: "maxage", maxAge: d}, nil
	}
	return datePat{}, fmt.Errorf("invalid date pattern: %q (future|before:YYYYMMDD|after:|between:,|max-age:)", pat)
}

var durationRe = regexp.MustCompile(`^(\d+)(ns|µs|ms|s|m|h|d)?$`)

// parseDuration: time.ParseDuration does not support days, we add them here.
func parseDuration(s string) (time.Duration, error) {
	m := durationRe.FindStringSubmatch(strings.ToLower(strings.TrimSpace(s)))
	if m == nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	n, err := fmtParseInt(m[1])
	if err != nil {
		return 0, err
	}
	unit := m[2]
	if unit == "" {
		unit = "m"
	}
	switch unit {
	case "d":
		return time.Duration(n) * 24 * time.Hour, nil
	default:
		return time.ParseDuration(s)
	}
}

func fmtParseInt(s string) (int64, error) {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid number %q", s)
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

// parseDate convierte YYYY-MM-DD a Unix (medianoche UTC).
func parseDate(s string) (int64, error) {
	t, err := time.ParseInLocation("2006-01-02", s, time.UTC)
	if err != nil {
		return 0, fmt.Errorf("invalid date %q (format YYYY-MM-DD)", s)
	}
	return t.Unix(), nil
}
