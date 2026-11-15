package api

import (
	"errors"
	"net/http"

	"github.com/gnacho/ocapps/internal/news/rules"
	"github.com/gnacho/ocapps/internal/news/store"
)

// rulesBody: block/keep rules in "Field=regex" multiline text.
type rulesBody struct {
	Block string `json:"block"`
	Keep  string `json:"keep"`
}

// validateRules compiles the rules to validate, 422 error with the i18n key
// invalid_rule if any line is invalid.
func validateRules(body rulesBody) bool {
	_, err := rules.Parse(body.Block, body.Keep)
	return err == nil
}

func (s *Server) getFeedRules(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "feedId")
	if !ok {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	}
	if _, err := s.store.GetFeed(user(r).ID, id); errors.Is(err, store.ErrNotFound) {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	}
	rs, err := s.store.GetFeedRules(id)
	if err != nil {
		s.logError(w, r, "read feed rules", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rs})
}

// setFeedRules stores the feed rules and re-applies them to its items.
func (s *Server) setFeedRules(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "feedId")
	if !ok {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	}
	if _, err := s.store.GetFeed(user(r).ID, id); errors.Is(err, store.ErrNotFound) {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	}
	var body rulesBody
	if err := decodeBody(r, &body); err != nil {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_body")
		return
	}
	if !validateRules(body) {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_rule")
		return
	}
	if err := s.store.SaveFeedRules(id, store.Rules{Block: body.Block, Keep: body.Keep}); err != nil {
		s.logError(w, r, "store feed rules", err)
		return
	}
	if _, err := s.store.ReapplyFeedFilter(id, store.FeedFilter{}); err != nil {
		s.logError(w, r, "re-apply feed rules", err)
		return
	}
	saved, err := s.store.GetFeedRules(id)
	if err != nil {
		s.logError(w, r, "read stored rules", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": saved})
}

// deleteFeedRules deletes the feed rules and unfreezes its items.
func (s *Server) deleteFeedRules(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "feedId")
	if !ok {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	}
	if _, err := s.store.GetFeed(user(r).ID, id); errors.Is(err, store.ErrNotFound) {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	}
	if err := s.store.DeleteFeedRules(id); err != nil {
		s.logError(w, r, "delete feed rules", err)
		return
	}
	if _, err := s.store.ReapplyFeedFilter(id, store.FeedFilter{}); err != nil {
		s.logError(w, r, "unfreeze feed items", err)
		return
	}
	writeEmpty(w)
}

// myRules: user global rules (GET /api/me/rules).
func (s *Server) myRules(w http.ResponseWriter, r *http.Request) {
	rs, err := s.store.GetGlobalRules(user(r).ID)
	if err != nil {
		s.logError(w, r, "read global rules", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rs})
}

// updateMyRules stores the global rules and re-applies them to all the user
// items (PUT /api/me/rules).
func (s *Server) updateMyRules(w http.ResponseWriter, r *http.Request) {
	var body rulesBody
	if err := decodeBody(r, &body); err != nil {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_body")
		return
	}
	if !validateRules(body) {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_rule")
		return
	}
	u := user(r)
	if err := s.store.SaveGlobalRules(u.ID, store.Rules{Block: body.Block, Keep: body.Keep}); err != nil {
		s.logError(w, r, "store global rules", err)
		return
	}
	if _, err := s.store.ReapplyGlobalRules(u.ID); err != nil {
		s.logError(w, r, "re-apply global rules", err)
		return
	}
	saved, err := s.store.GetGlobalRules(u.ID)
	if err != nil {
		s.logError(w, r, "read stored global rules", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": saved})
}
