package api

import (
	"net/http"
)

// itemFull: GET /items/{itemId}/full - full body extracted from the original
// article (readability), cached in the DB. Feeds bring summaries, this
// endpoint returns the whole sanitized text, with images through the proxy.
func (s *Server) itemFull(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("itemId"))
	if err != nil || id <= 0 {
		errorStatus(w, r, http.StatusNotFound, "item_not_found")
		return
	}
	u := user(r)

	// cache: the content of a published article does not change
	if body, err := s.store.GetItemFull(id); err == nil && body != "" {
		writeJSON(w, http.StatusOK, map[string]string{"body": s.rewriteBody(body, s.linkHygiene(r))})
		return
	}

	itemURL, err := s.store.GetItemURL(u.ID, id)
	if err != nil {
		errorStatus(w, r, http.StatusNotFound, "item_not_found")
		return
	}
	// per-feed CSS extraction selector (#39): if the feed defines it, it is used
	// before readability (fallback if the selector fails).
	selector := ""
	if feedID, err := s.store.GetItemFeedID(u.ID, id); err == nil {
		if sel, err := s.store.GetFeedScraperSelector(feedID); err == nil {
			selector = sel
		}
	}
	body, err := s.extract.Article(r.Context(), itemURL, selector)
	if err != nil {
		// it is not a server error: the site does not allow extraction
		s.log.Warn("full extraction failed", "url", itemURL, "err", err)
		errorStatus(w, r, http.StatusUnprocessableEntity, "full_unavailable")
		return
	}
	_ = s.store.SaveItemFull(id, body)
	writeJSON(w, http.StatusOK, map[string]string{"body": s.rewriteBody(body, s.linkHygiene(r))})
}
