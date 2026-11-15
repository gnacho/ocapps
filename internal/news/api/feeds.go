package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	urlpkg "net/url"
	"time"

	"github.com/gnacho/ocapps/internal/news/feed"
	"github.com/gnacho/ocapps/internal/news/store"
)

type feedsResponse struct {
	Feeds        []store.Feed `json:"feeds"`
	StarredCount int64        `json:"starredCount"`
	NewestItemID int64        `json:"newestItemId,omitempty"`
}

func (s *Server) listFeeds(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	feeds, err := s.store.ListFeeds(u.ID)
	if err != nil {
		s.logError(w, r, "list feeds", err)
		return
	}
	for i := range feeds {
		feeds[i].FaviconLink = s.rewriteFavicon(feeds[i].URLHash)
	}
	starred, _ := s.store.StarredCount(u.ID)
	newest, _ := s.store.NewestItemID(u.ID)
	writeJSON(w, http.StatusOK, feedsResponse{feeds, starred, newest})
}

// createFeed subscribes a new feed: it validates, fetches (F0: in the request
// itself with a timeout) and persists feed+items. It accepts JSON (spec) and
// form-urlencoded/query (news-android sends @Field form).
func (s *Server) createFeed(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL      string `json:"url"`
		FolderID *int64 `json:"folderId"`
		Username string `json:"username"` // feed Basic auth (News spec + news-android)
		Password string `json:"password"`
	}
	if err := decodeBody(r, &body); err != nil {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_body")
		return
	}
	// fallback form-urlencoded / query (ParseForm cubre ambos)
	if body.URL == "" {
		if err := r.ParseForm(); err == nil {
			if v := r.Form.Get("url"); v != "" {
				body.URL = v
			}
			if v := r.Form.Get("folderId"); v != "" {
				var fid int64
				if _, err := fmt.Sscanf(v, "%d", &fid); err == nil && fid > 0 {
					body.FolderID = &fid
				}
			}
			if v := r.Form.Get("username"); v != "" {
				body.Username = v
			}
			if v := r.Form.Get("password"); v != "" {
				body.Password = v
			}
		}
	}
	if body.URL == "" {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_url")
		return
	}
	if body.FolderID != nil && *body.FolderID > 0 {
		exists, err := s.store.FolderExists(user(r).ID, *body.FolderID)
		if err != nil || !exists {
			errorStatus(w, r, http.StatusNotFound, "folder_not_found")
			return
		}
	}
	if exists, _ := s.store.FeedExistsByURL(user(r).ID, body.URL); exists {
		errorStatus(w, r, http.StatusConflict, "feed_exists")
		return
	}

	f, items, err := s.fetcher.Fetch(r.Context(), body.URL, body.Username, body.Password)
	if err != nil {
		if errors.Is(err, feed.ErrAuthRequired) {
			errorStatus(w, r, http.StatusUnprocessableEntity, "feed_auth_required")
			return
		}
		// spec: 422 if the feed cannot be read
		s.log.Warn("new feed fetch failed", "url", body.URL, "err", err)
		errorStatus(w, r, http.StatusUnprocessableEntity, "feed_unreadable")
		return
	}
	f.URL = body.URL // the subscription URL wins over the one declared in the XML
	if body.FolderID != nil && *body.FolderID > 0 {
		f.FolderID = body.FolderID
	}
	fullContent := feed.HasFullContent(items)
	feed.SanitizeItems(items) // the subscription items are cleaned too

	created, err := s.store.CreateFeedFull(user(r).ID, body.URL, f.FolderID, f.Title, f.Link, f.FaviconLink, items, fullContent)
	if errors.Is(err, store.ErrConflict) {
		errorStatus(w, r, http.StatusConflict, "feed_exists")
		return
	}
	if err != nil {
		s.logError(w, r, "create feed", err)
		return
	}
	if body.Username != "" {
		enc, err := s.cred.Encrypt(body.Password)
		if err != nil {
			s.logError(w, r, "encrypt credentials", err)
			return
		}
		if err := s.store.SetFeedCredentials(user(r).ID, created.ID, body.Username, &enc); err != nil {
			s.logError(w, r, "store credentials", err)
			return
		}
		created.AuthUser = body.Username
	}
	newest, _ := s.store.NewestItemID(user(r).ID)
	created.FaviconLink = s.rewriteFavicon(created.URLHash)
	s.log.Info("feed suscrito", "url", body.URL, "items", len(items))
	writeJSON(w, http.StatusOK, feedsResponse{[]store.Feed{*created}, 0, newest})
}

func (s *Server) deleteFeed(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "feedId")
	if !ok {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	}
	if err := s.store.DeleteFeed(user(r).ID, id); errors.Is(err, store.ErrNotFound) {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
	} else if err != nil {
		s.logError(w, r, "delete feed", err)
	} else {
		writeEmpty(w)
	}
}

func (s *Server) moveFeed(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "feedId")
	if !ok {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	}
	var body struct {
		FolderID *int64 `json:"folderId"`
	}
	if err := decodeBody(r, &body); err != nil {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_body")
		return
	}
	switch err := s.store.MoveFeed(user(r).ID, id, normFolderID(body.FolderID)); {
	case errors.Is(err, store.ErrNotFound):
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
	case err != nil:
		s.logError(w, r, "mover feed", err)
	default:
		writeEmpty(w)
	}
}

func (s *Server) renameFeed(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "feedId")
	if !ok {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	}
	var body struct {
		FeedTitle string `json:"feedTitle"`
	}
	if err := decodeBody(r, &body); err != nil || body.FeedTitle == "" {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_title")
		return
	}
	if err := s.store.RenameFeed(user(r).ID, id, body.FeedTitle); errors.Is(err, store.ErrNotFound) {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
	} else if err != nil {
		s.logError(w, r, "renombrar feed", err)
	} else {
		writeEmpty(w)
	}
}

func (s *Server) markFeedRead(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "feedId")
	if !ok {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	}
	maxID, ok := newestItemID(r, w)
	if !ok {
		return
	}
	if _, err := s.store.GetFeed(user(r).ID, id); errors.Is(err, store.ErrNotFound) {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	}
	if _, err := s.store.MarkAllRead(user(r).ID, maxID, "feed", id); err != nil {
		s.logError(w, r, "mark feed read", err)
		return
	}
	writeEmpty(w)
}

// updateFeed: official updater API (GET /feeds/update?userId=&feedId=).
// It requires the admin role, it refetches the given feed.
func (s *Server) updateFeed(w http.ResponseWriter, r *http.Request) {
	if user(r).Role != "admin" {
		errorStatus(w, r, http.StatusUnauthorized, "admin_required")
		return
	}
	q := r.URL.Query()
	username := q.Get("userId")
	feedID, err := parseID(q.Get("feedId"))
	if err != nil || feedID <= 0 {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	}
	owner, err := s.store.GetUserByUsername(username)
	if err != nil {
		errorStatus(w, r, http.StatusNotFound, "user_not_found")
		return
	}
	f, err := s.store.GetFeed(owner.ID, feedID)
	if err != nil {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	}
	if res := s.refresher.Refresh(r.Context(), f); res.Err != nil {
		s.logError(w, r, "update feed", res.Err)
		return
	}
	writeEmpty(w)
}

// discoverFeed: feed autodiscovery on a site (GET /feeds/discover?url=).
// Extension own route (the v1.3 spec does not define it).
func (s *Server) discoverFeed(w http.ResponseWriter, r *http.Request) {
	u := r.URL.Query().Get("url")
	if u == "" {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_url")
		return
	}
	parsed, err := urlpkg.Parse(u)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_url")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	feeds, err := s.fetcher.Discover(ctx, u)
	if err != nil {
		s.log.Warn("discover failed", "url", u, "err", err)
		errorStatus(w, r, http.StatusUnprocessableEntity, "no_feeds_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"feeds": feeds})
}

// normFolderID normalizes folderId 0 -> nil (root).
func normFolderID(id *int64) *int64 {
	if id != nil && *id == 0 {
		return nil
	}
	return id
}

// getFeedRetention returns the retention override of the feed (0 = global).
func (s *Server) getFeedRetention(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "feedId")
	if !ok {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	}
	f, err := s.store.GetFeed(user(r).ID, id)
	if errors.Is(err, store.ErrNotFound) {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	}
	if err != nil {
		s.logError(w, r, "read feed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"feedId": id, "retentionDays": f.RetentionDays})
}

// setFeedRetention sets the retention override (POST /feeds/{id}/retention).
func (s *Server) setFeedRetention(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "feedId")
	if !ok {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	}
	var body struct {
		RetentionDays int64 `json:"retentionDays"`
	}
	if err := decodeBody(r, &body); err != nil {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_body")
		return
	}
	if body.RetentionDays < 0 || body.RetentionDays > 3650 {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_retention")
		return
	}
	if err := s.store.SetFeedRetentionDays(id, user(r).ID, body.RetentionDays); errors.Is(err, store.ErrNotFound) {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	} else if err != nil {
		s.logError(w, r, "set retention", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"feedId": id, "retentionDays": body.RetentionDays})
}

// setFeedCredentials sets or removes the feed Basic auth
// (POST /feeds/{id}/credentials). Both fields empty = remove, empty
// password with a username = keep the current password. The password is
// stored encrypted (internal/cred) and NEVER returned by the API.
func (s *Server) setFeedCredentials(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "feedId")
	if !ok {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeBody(r, &body); err != nil {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_body")
		return
	}
	u := user(r)
	if body.Username == "" && body.Password == "" {
		empty := ""
		err := s.store.SetFeedCredentials(u.ID, id, "", &empty)
		if errors.Is(err, store.ErrNotFound) {
			errorStatus(w, r, http.StatusNotFound, "feed_not_found")
			return
		}
		if err != nil {
			s.logError(w, r, "remove credentials", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"feedId": id, "authUser": ""})
		return
	}
	if body.Username == "" {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_credentials")
		return
	}
	var passEnc *string
	if body.Password != "" {
		enc, err := s.cred.Encrypt(body.Password)
		if err != nil {
			s.logError(w, r, "encrypt credentials", err)
			return
		}
		passEnc = &enc
	}
	if err := s.store.SetFeedCredentials(u.ID, id, body.Username, passEnc); errors.Is(err, store.ErrNotFound) {
		errorStatus(w, r, http.StatusNotFound, "feed_not_found")
		return
	} else if err != nil {
		s.logError(w, r, "store credentials", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"feedId": id, "authUser": body.Username})
}
