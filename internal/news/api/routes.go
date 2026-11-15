package api

import "net/http"

func (s *Server) routes(mux *http.ServeMux) {
	// Misc
	mux.HandleFunc("GET /version", s.handleVersion)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /user", s.handleUser)

	// Folders
	mux.HandleFunc("GET /folders", s.listFolders)
	mux.HandleFunc("POST /folders", s.createFolder)
	mux.HandleFunc("PUT /folders/{folderId}", s.renameFolder)
	mux.HandleFunc("DELETE /folders/{folderId}", s.deleteFolder)
	mux.HandleFunc("POST /folders/{folderId}/read", s.markFolderRead)

	// Feeds
	mux.HandleFunc("GET /feeds", s.listFeeds)
	mux.HandleFunc("POST /feeds", s.createFeed)
	mux.HandleFunc("GET /feeds/discover", s.discoverFeed)
	mux.HandleFunc("DELETE /feeds/{feedId}", s.deleteFeed)
	// move/rename: the v1.3 spec says POST but news-android uses PUT -> both
	for _, m := range []string{http.MethodPost, http.MethodPut} {
		mux.HandleFunc(m+" /feeds/{feedId}/move", s.moveFeed)
		mux.HandleFunc(m+" /feeds/{feedId}/rename", s.renameFeed)
	}
	mux.HandleFunc("POST /feeds/{feedId}/read", s.markFeedRead)
	mux.HandleFunc("GET /feeds/{feedId}/filter", s.getFeedFilter)
	mux.HandleFunc("POST /feeds/{feedId}/filter", s.setFeedFilter)
	mux.HandleFunc("DELETE /feeds/{feedId}/filter", s.deleteFeedFilter)
	mux.HandleFunc("GET /feeds/{feedId}/rules", s.getFeedRules)
	mux.HandleFunc("POST /feeds/{feedId}/rules", s.setFeedRules)
	mux.HandleFunc("DELETE /feeds/{feedId}/rules", s.deleteFeedRules)
	mux.HandleFunc("GET /feeds/{feedId}/retention", s.getFeedRetention)
	mux.HandleFunc("POST /feeds/{feedId}/retention", s.setFeedRetention)
	mux.HandleFunc("GET /feeds/{feedId}/scraper", s.getFeedScraper)
	mux.HandleFunc("POST /feeds/{feedId}/scraper", s.setFeedScraper)
	mux.HandleFunc("POST /feeds/{feedId}/credentials", s.setFeedCredentials)
	mux.HandleFunc("GET /feeds/update", s.updateFeed) // updater API, admin only

	// Items
	mux.HandleFunc("GET /items", s.listItems)
	mux.HandleFunc("GET /items/updated", s.updatedItems)
	mux.HandleFunc("GET /items/search", s.searchItems)
	mux.HandleFunc("GET /items/{itemId}/full", s.itemFull)
	mux.HandleFunc("POST /items/read", s.markAllRead)
	mux.HandleFunc("POST /items/{itemId}/read", s.markItem(false, "unread"))
	mux.HandleFunc("POST /items/{itemId}/unread", s.markItem(true, "unread"))
	mux.HandleFunc("POST /items/{itemId}/star", s.markItem(true, "starred"))
	mux.HandleFunc("POST /items/{itemId}/unstar", s.markItem(false, "starred"))
	mux.HandleFunc("POST /items/{itemId}/share", s.createShare)
	mux.HandleFunc("DELETE /items/{itemId}/share", s.deleteShare)

	// Bulk marking: the official spec contradicts itself (definitions: POST with
	// "itemIds", How To Sync section: PUT with "items") -> we register both
	// methods and accept both keys.
	for _, m := range []string{http.MethodPost, http.MethodPut} {
		mux.HandleFunc(m+" /items/read/multiple", s.markMultiple(false, "unread"))
		mux.HandleFunc(m+" /items/unread/multiple", s.markMultiple(true, "unread"))
		mux.HandleFunc(m+" /items/star/multiple", s.markMultiple(true, "starred"))
		mux.HandleFunc(m+" /items/unstar/multiple", s.markMultiple(false, "starred"))
	}

	// Favicon (v1.3 spec, News 27.2+): md5 hash of the feed URL.
	// It is registered as a PUBLIC route in Handler() (it does not require auth: it only
	// reads from the on-disk cache, no SSRF).

	// Updater API (spec): cleanup + feeds/all, admin only.
	mux.HandleFunc("GET /cleanup/before-update", s.adminOnly(s.cleanupBefore))
	mux.HandleFunc("GET /cleanup/after-update", s.adminOnly(s.cleanupAfter))
	mux.HandleFunc("GET /feeds/all", s.adminOnly(s.allFeedsForUpdater))

	// OPML (app own route, not in the v1.3 spec): export/import of
	// subscriptions for the PWA and migration from other readers.
	mux.HandleFunc("GET /export/opml", s.exportOPML)
	mux.HandleFunc("POST /import/opml", s.importOPML)

	// Manual refresh (own extension under the v1.3 base, the spec updater
	// route is admin only). NOTE: NOTHING under /api/, that prefix belongs to
	// OpenCloud (api/v0/settings) and cannot be proxied to ocnews.
	mux.HandleFunc("POST /refresh", s.refreshUserFeeds)

	// Saved searches (own extension, virtual feed).
	mux.HandleFunc("GET /searches", s.listSearches)
	mux.HandleFunc("POST /searches", s.createSearch)
	mux.HandleFunc("DELETE /searches/{searchId}", s.deleteSearch)
	mux.HandleFunc("GET /searches/{searchId}/items", s.searchSavedItems)

	// Auto-mark as read on arrival (own extension).
	mux.HandleFunc("GET /auto-read", s.listAutoRead)
	mux.HandleFunc("POST /auto-read", s.addAutoRead)
	mux.HandleFunc("DELETE /auto-read/{ruleId}", s.deleteAutoRead)
}

// adminOnly wraps a handler requiring the admin role.
func (s *Server) adminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if user(r).Role != "admin" {
			errorStatus(w, r, http.StatusUnauthorized, "admin_required")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": reportedVersion})
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version": reportedVersion,
		"warnings": map[string]bool{
			"improperlyConfiguredCron": false,
			"incorrectDbCharset":       false,
		},
	})
}

func (s *Server) handleUser(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"userId":             u.Username,
		"displayName":        u.DisplayName,
		"lastLoginTimestamp": u.LastLoginAt,
		"language":           u.Language, // own extension, the spec allows new attributes
		"avatar":             nil,
	})
}
