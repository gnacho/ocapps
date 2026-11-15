package api

import (
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gnacho/ocapps/internal/news/store"
)

// serveFavicon: GET /favicon/{feedUrlHash} - it responds from the cache.
// We only serve if the hash corresponds to an existing feed.
func (s *Server) serveFavicon(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if hash == "" || s.favicons == nil {
		http.NotFound(w, r)
		return
	}
	if _, err := s.store.FeedByURLHash(hash); err != nil {
		http.NotFound(w, r)
		return
	}
	s.favicons.Serve(w, hash)
}

// faviconRouter serves the favicon through a public route (no auth). The browser
// <img> carries no credentials, it only reads from the on-disk cache.
func (s *Server) faviconRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /favicon/{hash}", s.serveFavicon)
	return mux
}

// cleanupBefore: spec - deletes folders/feeds flagged for deletion. In our
// implementation the DELETEs are already immediate, it remains as a contract no-op.
func (s *Server) cleanupBefore(w http.ResponseWriter, _ *http.Request) {
	writeEmpty(w)
}

// cleanupAfter: spec - purges read non-starred items according to retention.
func (s *Server) cleanupAfter(w http.ResponseWriter, r *http.Request) {
	if s.retention > 0 {
		cutoff := time.Now().Add(-s.retention).Unix()
		if n, err := s.store.PurgeOldItems(cutoff); err != nil {
			s.logError(w, r, "cleanup after-update", err)
			return
		} else if n > 0 {
			s.log.Info("cleanup after-update", "items", n)
		}
	}
	writeEmpty(w)
}

// allFeedsForUpdater: spec - feeds with userId for the external updater.
func (s *Server) allFeedsForUpdater(w http.ResponseWriter, r *http.Request) {
	feeds, err := s.store.AllFeedsWithUser()
	if err != nil {
		s.logError(w, r, "feeds/all", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"feeds": feeds})
}

// ---- OPML ----

type opmlOutline struct {
	XMLName  xml.Name      `xml:"outline"`
	Text     string        `xml:"text,attr"`
	Title    string        `xml:"title,attr,omitempty"`
	Type     string        `xml:"type,attr,omitempty"`
	XMLURL   string        `xml:"xmlUrl,attr,omitempty"`
	Outlines []opmlOutline `xml:"outline"`
}

type opmlDoc struct {
	XMLName  xml.Name      `xml:"opml"`
	Version  string        `xml:"version,attr"`
	Title    string        `xml:"head>title"`
	Outlines []opmlOutline `xml:"body>outline"`
}

// exportOPML: GET /export/opml - user subscriptions in OPML 2.0.
func (s *Server) exportOPML(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	folders, err := s.store.ListFolders(u.ID)
	if err != nil {
		s.logError(w, r, "exportar opml", err)
		return
	}
	feeds, err := s.store.ListFeeds(u.ID)
	if err != nil {
		s.logError(w, r, "exportar opml", err)
		return
	}

	byFolder := map[int64][]opmlOutline{}
	var root []opmlOutline
	for _, f := range feeds {
		o := opmlOutline{Text: f.Title, Title: f.Title, Type: "rss", XMLURL: f.URL}
		if f.FolderID != nil {
			byFolder[*f.FolderID] = append(byFolder[*f.FolderID], o)
		} else {
			root = append(root, o)
		}
	}
	for _, fo := range folders {
		children := byFolder[fo.ID]
		if children == nil {
			children = []opmlOutline{}
		}
		root = append(root, opmlOutline{Text: fo.Name, Title: fo.Name, Outlines: children})
	}

	doc := opmlDoc{Version: "2.0", Title: "ocnews suscripciones", Outlines: root}
	w.Header().Set("Content-Type", "text/x-opml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="subscriptions.opml"`)
	w.WriteHeader(http.StatusOK)
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		s.log.Error("encode opml", "err", err)
	}
}

type importedFeed struct {
	URL        string
	FolderName string
}

// parseOPML extracts feeds (and their containing folder) from an OPML document.
// Tolerant: nested folders are flattened (the model only has 1 level).
func parseOPML(body []byte) []importedFeed {
	var doc opmlDoc
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil
	}
	var out []importedFeed
	var walk func(outlines []opmlOutline, folder string)
	walk = func(outlines []opmlOutline, folder string) {
		for _, o := range outlines {
			if o.XMLURL != "" {
				name := folder
				out = append(out, importedFeed{URL: strings.TrimSpace(o.XMLURL), FolderName: name})
				continue
			}
			f := o.Text
			if f == "" {
				f = o.Title
			}
			walk(o.Outlines, f)
		}
	}
	walk(doc.Outlines, "")
	return out
}

// importOPML: POST /import/opml - imports subscriptions (it creates folders,
// feeds without fetching, the scheduler refreshes them in the next cycle since
// next_update=0).
func (s *Server) importOPML(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_body")
		return
	}
	imports := parseOPML(body)
	if len(imports) == 0 {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_body")
		return
	}

	created, skipped := 0, 0
	for _, imp := range imports {
		var folderID *int64
		if imp.FolderName != "" {
			f, err := s.store.CreateFolder(u.ID, imp.FolderName, nil)
			if err == store.ErrConflict {
				existing, err2 := s.store.ListFolders(u.ID)
				if err2 == nil {
					for _, fo := range existing {
						if fo.Name == imp.FolderName {
							id := fo.ID
							folderID = &id
							break
						}
					}
				}
			} else if err == nil {
				folderID = &f.ID
			}
		}
		if exists, _ := s.store.FeedExistsByURL(u.ID, imp.URL); exists {
			skipped++
			continue
		}
		// feed without an initial fetch: the scheduler rescues it (next_update 0)
		if _, err := s.store.CreateFeedDeferred(u.ID, imp.URL, folderID); err == nil {
			created++
		} else {
			skipped++
		}
	}
	s.log.Info("opml importado", "creados", created, "omitidos", skipped)
	writeJSON(w, http.StatusOK, map[string]int{"imported": created, "skipped": skipped})
}
