package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gnacho/ocapps/internal/news/store"
)

// filterBody: comma separated keywords (News 28.4.0).
type filterBody struct {
	TitleKeywords string `json:"titleKeywords"`
	BodyKeywords  string `json:"bodyKeywords"`
	URLKeywords   string `json:"urlKeywords"`
}

// sanitizeKeywords trims and normalizes (no trailing spaces per keyword).
func sanitizeKeywords(s string) string {
	parts := strings.Split(s, ",")
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ",")
}

func (b filterBody) toStore(feedID int64) store.FeedFilter {
	return store.FeedFilter{
		FeedID:        feedID,
		TitleKeywords: sanitizeKeywords(b.TitleKeywords),
		BodyKeywords:  sanitizeKeywords(b.BodyKeywords),
		URLKeywords:   sanitizeKeywords(b.URLKeywords),
	}
}

// getFeedFilter returns the feed filter (GET /feeds/{id}/filter).
func (s *Server) getFeedFilter(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "feedId")
	if !ok {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	}
	if _, err := s.store.GetFeed(user(r).ID, id); errors.Is(err, store.ErrNotFound) {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	}
	f, err := s.store.GetFeedFilter(id)
	if err != nil {
		s.logError(w, r, "read filter", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"filter": f})
}

// setFeedFilter creates or updates the filter (POST /feeds/{id}/filter).
// Empty keywords -> the filter is deleted. It re-applies over the items already stored.
func (s *Server) setFeedFilter(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "feedId")
	if !ok {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	}
	if _, err := s.store.GetFeed(user(r).ID, id); errors.Is(err, store.ErrNotFound) {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	}
	var body filterBody
	if err := decodeBody(r, &body); err != nil {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_body")
		return
	}
	f := body.toStore(id)
	if err := s.store.SaveFeedFilter(f); err != nil {
		s.logError(w, r, "store filter", err)
		return
	}
	if _, err := s.store.ReapplyFeedFilter(id, f); err != nil {
		s.logError(w, r, "re-apply filter", err)
		return
	}
	saved, err := s.store.GetFeedFilter(id)
	if err != nil {
		s.logError(w, r, "read stored filter", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"filter": saved})
}

// deleteFeedFilter deletes the filter and unfreezes the items (DELETE /feeds/{id}/filter).
func (s *Server) deleteFeedFilter(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "feedId")
	if !ok {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	}
	if _, err := s.store.GetFeed(user(r).ID, id); errors.Is(err, store.ErrNotFound) {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	}
	if err := s.store.DeleteFeedFilter(id); err != nil {
		s.logError(w, r, "delete filter", err)
		return
	}
	// unfreeze items previously filtered from this feed
	if _, err := s.store.ReapplyFeedFilter(id, store.FeedFilter{}); err != nil {
		s.logError(w, r, "unfreeze items", err)
		return
	}
	writeEmpty(w)
}
