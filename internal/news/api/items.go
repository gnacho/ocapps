package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gnacho/ocapps/internal/news/store"
)

// parseID converts a string id to int64 (>0).
func parseID(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// boolParam reads a boolean from the query (true|1).
func boolParam(r *http.Request, key string, def bool) bool {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	return v == "true" || v == "1"
}

// newestItemID reads {"newestItemId": N} from the body (0 = no cap).
func newestItemID(r *http.Request, w http.ResponseWriter) (int64, bool) {
	var body struct {
		NewestItemID int64 `json:"newestItemId"`
	}
	if err := decodeBody(r, &body); err != nil {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_body")
		return 0, false
	}
	return body.NewestItemID, true
}

// itemFilterFromRequest builds the common filter of /items and /items/updated.
func itemFilterFromRequest(r *http.Request, u *store.User, updated bool) (store.ItemFilter, bool) {
	q := r.URL.Query()
	f := store.ItemFilter{UserID: u.ID}

	if v, err := parseID(q.Get("type")); err == nil {
		f.Type = int(v)
	} else {
		f.Type = 3
	}
	if f.Type < 0 || f.Type > 3 {
		return f, false
	}
	if f.Type != 2 && f.Type != 3 {
		if v, err := parseID(q.Get("id")); err == nil {
			f.ID = v
		}
	}
	if updated {
		if v, err := parseID(q.Get("lastModified")); err == nil {
			f.UpdatedSince = v
		}
		f.GetRead = true         // updated reports state changes, read items included
		f.BatchSize = -1         // the spec does not define batchSize in /items/updated
		f.IncludeFiltered = true // updated also reports the filtered state
	} else {
		f.GetRead = boolParam(r, "getRead", true)
		f.IncludeFiltered = boolParam(r, "getFiltered", false)
		if v, err := parseID(q.Get("batchSize")); err == nil {
			f.BatchSize = v
		} else {
			f.BatchSize = -1
		}
		if v, err := parseID(q.Get("offset")); err == nil {
			f.OffsetID = v
		}
		f.OldestFirst = boolParam(r, "oldestFirst", false)
	}
	return f, true
}

// annotateClusters fills clusterSize/clusterPrimaryId of the listed items
// (#42): it queries the cluster_keys of the page and their global size per user.
func (s *Server) annotateClusters(u *store.User, items []store.Item) {
	if len(items) == 0 {
		return
	}
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	keysByID, err := s.store.ItemClusterKeys(u.ID, ids)
	if err != nil {
		return
	}
	seen := map[string]bool{}
	keys := []string{}
	for _, k := range keysByID {
		if k != "" && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	clusters, err := s.store.Clusters(u.ID, keys)
	if err != nil {
		return
	}
	for i := range items {
		if k := keysByID[items[i].ID]; k != "" {
			if c, ok := clusters[k]; ok {
				items[i].ClusterSize = c.Size
				items[i].ClusterPrimaryID = c.PrimaryID
			}
		}
	}
}

func (s *Server) listItems(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	f, valid := itemFilterFromRequest(r, u, false)
	if !valid {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_type")
		return
	}
	items, err := s.store.ListItems(f)
	if err != nil {
		s.logError(w, r, "list items", err)
		return
	}
	s.annotateClusters(u, items)
	s.rewriteAll(items)
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) updatedItems(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	f, valid := itemFilterFromRequest(r, u, true)
	if !valid {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_type")
		return
	}
	items, err := s.store.ListItems(f)
	if err != nil {
		s.logError(w, r, "updated items", err)
		return
	}
	s.annotateClusters(u, items)
	s.rewriteAll(items)
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// searchItems: article search by query in title/body/URL.
// App own route under the v1-3 base (the v1.3 spec does not define search).
func (s *Server) searchItems(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := strings.TrimSpace(q.Get("query"))
	if query == "" {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_query")
		return
	}
	u := user(r)
	f, valid := itemFilterFromRequest(r, u, false)
	if !valid {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_type")
		return
	}
	// search: a reasonable limit (clients paginate from there)
	if f.BatchSize < 0 || f.BatchSize > 200 {
		f.BatchSize = 100
	}
	items, err := s.store.SearchItems(f, query, int(f.BatchSize))
	if err != nil {
		s.logError(w, r, "buscar items", err)
		return
	}
	s.annotateClusters(u, items)
	s.rewriteAll(items)
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// markItem returns a handler to mark an individual item.
// val=true -> unread/starred according to col, false -> it removes it.
func (s *Server) markItem(val bool, col string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseID(r.PathValue("itemId"))
		if err != nil || id <= 0 {
			errorStatus(w, r, http.StatusNotFound, "item_not_found")
			return
		}
		u := user(r)
		exists, err := s.store.ItemExists(u.ID, id)
		if err != nil {
			s.logError(w, r, "check item", err)
			return
		}
		if !exists {
			errorStatus(w, r, http.StatusNotFound, "item_not_found")
			return
		}
		var mErr error
		if col == "unread" {
			mErr = s.store.MarkItemsUnreadFlag(u.ID, []int64{id}, val)
		} else {
			mErr = s.store.MarkItemsStarFlag(u.ID, []int64{id}, val)
		}
		if mErr != nil {
			s.logError(w, r, "mark item", mErr)
			return
		}
		writeEmpty(w)
	}
}

// multipleBody accepts "itemIds" (spec definitions) and "items"
// (How To Sync section): real clients send both forms.
type multipleBody struct {
	ItemIDs []int64 `json:"itemIds"`
	Items   []int64 `json:"items"`
}

func (b multipleBody) ids() []int64 {
	return append(b.ItemIDs, b.Items...)
}

func (s *Server) markMultiple(val bool, col string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body multipleBody
		if err := decodeBody(r, &body); err != nil {
			errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_body")
			return
		}
		ids := body.ids()
		if len(ids) == 0 {
			writeEmpty(w)
			return
		}
		var err error
		if col == "unread" {
			err = s.store.MarkItemsUnreadFlag(user(r).ID, ids, val)
		} else {
			err = s.store.MarkItemsStarFlag(user(r).ID, ids, val)
		}
		if err != nil {
			s.logError(w, r, "mark items", err)
			return
		}
		writeEmpty(w)
	}
}

// markAllRead marks all the user items up to newestItemId.
func (s *Server) markAllRead(w http.ResponseWriter, r *http.Request) {
	maxID, ok := newestItemID(r, w)
	if !ok {
		return
	}
	if _, err := s.store.MarkAllRead(user(r).ID, maxID, "all", 0); err != nil {
		s.logError(w, r, "mark all read", err)
		return
	}
	writeEmpty(w)
}
