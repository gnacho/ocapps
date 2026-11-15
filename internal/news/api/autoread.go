package api

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/gnacho/ocapps/internal/news/store"
)

// listAutoRead: user auto-mark rules (GET /auto-read).
func (s *Server) listAutoRead(w http.ResponseWriter, r *http.Request) {
	rules, err := s.store.ListAutoRead(user(r).ID)
	if err != nil {
		s.logError(w, r, "list auto-mark rules", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

// addAutoRead: creates an auto-mark rule (POST /auto-read).
func (s *Server) addAutoRead(w http.ResponseWriter, r *http.Request) {
	var body struct {
		FeedID       int64  `json:"feedId"`
		TitlePattern string `json:"titlePattern"`
	}
	if err := decodeBody(r, &body); err != nil {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_body")
		return
	}
	body.TitlePattern = strings.TrimSpace(body.TitlePattern)
	if body.TitlePattern == "" {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_rule")
		return
	}
	if _, err := regexp.Compile(body.TitlePattern); err != nil {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_rule")
		return
	}
	u := user(r)
	rule, err := s.store.AddAutoRead(u.ID, body.FeedID, body.TitlePattern)
	if errors.Is(err, store.ErrNotFound) {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	}
	if err != nil {
		s.logError(w, r, "create auto-mark rule", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"rule": rule})
}

// deleteAutoRead: deletes an auto-mark rule (DELETE /auto-read/{ruleId}).
func (s *Server) deleteAutoRead(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "ruleId")
	if !ok {
		errorStatus(w, r, http.StatusNotFound, "rule_not_found")
		return
	}
	if err := s.store.DeleteAutoRead(user(r).ID, id); errors.Is(err, store.ErrNotFound) {
		errorStatus(w, r, http.StatusNotFound, "rule_not_found")
		return
	} else if err != nil {
		s.logError(w, r, "delete auto-mark rule", err)
		return
	}
	writeEmpty(w)
}
