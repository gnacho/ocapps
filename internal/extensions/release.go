package extensions

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sync"
	"time"
)

// Release is the newest published release of an app that carries a matching
// asset.
type Release struct {
	Tag      string
	AssetURL string
}

// GitHub resolves the latest release for each app. Results are cached: the
// panel may be opened often and the unauthenticated API allows 60 requests
// per hour.
type GitHub struct {
	Client  *http.Client
	Token   string
	TTL     time.Duration
	BaseURL string
	now     func() time.Time

	mu    sync.Mutex
	cache map[string]cachedRelease
}

type cachedRelease struct {
	rel     Release
	err     error
	expires time.Time
}

func NewGitHub(client *http.Client, token string, ttl time.Duration) *GitHub {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &GitHub{Client: client, Token: token, TTL: ttl, BaseURL: "https://api.github.com", now: time.Now, cache: map[string]cachedRelease{}}
}

// Latest returns the newest release whose assets contain one matching re.
// The API is asked for the recent releases and the first match by publication
// date wins, because several of these repos publish more than one artifact
// (ocnews also ships the collector binaries).
func (g *GitHub) Latest(ctx context.Context, repo string, re *regexp.Regexp) (Release, error) {
	key := repo + " " + re.String()
	g.mu.Lock()
	if c, ok := g.cache[key]; ok && g.now().Before(c.expires) {
		g.mu.Unlock()
		return c.rel, c.err
	}
	g.mu.Unlock()

	rel, err := g.fetch(ctx, repo, re)

	g.mu.Lock()
	g.cache[key] = cachedRelease{rel: rel, err: err, expires: g.now().Add(g.TTL)}
	g.mu.Unlock()
	return rel, err
}

func (g *GitHub) fetch(ctx context.Context, repo string, re *regexp.Regexp) (Release, error) {
	url := g.BaseURL + "/repos/" + repo + "/releases?per_page=25"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}

	resp, err := g.Client.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("github: %s", resp.Status)
	}

	var payload []struct {
		TagName     string    `json:"tag_name"`
		Draft       bool      `json:"draft"`
		PublishedAt time.Time `json:"published_at"`
		Assets      []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return Release{}, err
	}

	best := Release{}
	var bestAt time.Time
	for _, r := range payload {
		if r.Draft {
			continue
		}
		for _, a := range r.Assets {
			if !re.MatchString(a.Name) {
				continue
			}
			if r.PublishedAt.After(bestAt) {
				bestAt = r.PublishedAt
				best = Release{Tag: r.TagName, AssetURL: a.URL}
			}
			break
		}
	}
	if best.Tag == "" {
		return Release{}, fmt.Errorf("%s: no release with a matching asset", repo)
	}
	return best, nil
}
