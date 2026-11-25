package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gnacho/ocapps/internal/news/store"
)

// listSearches: user saved searches (GET /searches).
func (s *Server) listSearches(w http.ResponseWriter, r *http.Request) {
	searches, err := s.store.ListSavedSearches(user(r).ID)
	if err != nil {
		s.logError(w, r, "list saved searches", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"searches": searches})
}

// createSearch: stores a search (POST /searches).
func (s *Server) createSearch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name  string `json:"name"`
		Query string `json:"query"`
	}
	if err := decodeBody(r, &body); err != nil {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_body")
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	body.Query = strings.TrimSpace(body.Query)
	if body.Name == "" || body.Query == "" {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_search")
		return
	}
	ss, err := s.store.CreateSavedSearch(user(r).ID, body.Name, body.Query)
	if err != nil {
		s.logError(w, r, "create saved search", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"search": ss})
}

// deleteSearch: deletes a saved search (DELETE /searches/{searchId}).
func (s *Server) deleteSearch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "searchId")
	if !ok {
		errorStatus(w, r, http.StatusNotFound, "search_not_found")
		return
	}
	if err := s.store.DeleteSavedSearch(user(r).ID, id); errors.Is(err, store.ErrNotFound) {
		errorStatus(w, r, http.StatusNotFound, "search_not_found")
		return
	} else if err != nil {
		s.logError(w, r, "delete saved search", err)
		return
	}
	writeEmpty(w)
}

// searchSavedItems: runs the saved search (GET /searches/{searchId}/items).
func (s *Server) searchSavedItems(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "searchId")
	if !ok {
		errorStatus(w, r, http.StatusNotFound, "search_not_found")
		return
	}
	u := user(r)
	ss, err := s.store.GetSavedSearch(u.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		errorStatus(w, r, http.StatusNotFound, "search_not_found")
		return
	}
	if err != nil {
		s.logError(w, r, "read saved search", err)
		return
	}
	f, valid := itemFilterFromRequest(r, u, false)
	if !valid {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_type")
		return
	}
	if f.BatchSize < 0 || f.BatchSize > 200 {
		f.BatchSize = 100
	}
	items, err := s.store.SearchItems(f, ss.Query, int(f.BatchSize))
	if err != nil {
		s.logError(w, r, "search items of the saved search", err)
		return
	}
	s.annotateClusters(u, items)
	s.rewriteAll(items, s.linkHygiene(r))
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
