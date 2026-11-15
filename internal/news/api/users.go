package api

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/crypto/bcrypt"

	"github.com/gnacho/ocapps/internal/news/auth"
	"github.com/gnacho/ocapps/internal/news/store"
)

// userAPIRoute: an exact pattern of the own API and its handler. path is the
// pattern without method (to also register the OPTIONS preflight).
type userAPIRoute struct {
	pattern string
	path    string
	h       http.HandlerFunc
}

// userAPIRoutes: app OWN endpoints (not from the v1.3 spec) for the
// PWA and the extension: own profile and user management. They are EXACT
// PATTERNS (SPEC §4.1: no /api/ catch-all in the unified mux).
func (s *Server) userAPIRoutes() []userAPIRoute {
	return []userAPIRoute{
		{"GET /api/me", "/api/me", s.me},
		{"PUT /api/me", "/api/me", s.updateMe},
		{"PUT /api/me/password", "/api/me/password", s.changeMyPassword},
		{"GET /api/me/settings", "/api/me/settings", s.mySettings},
		{"PUT /api/me/settings", "/api/me/settings", s.updateMySettings},
		{"GET /api/me/rules", "/api/me/rules", s.myRules},
		{"PUT /api/me/rules", "/api/me/rules", s.updateMyRules},
		{"GET /api/users", "/api/users", s.adminOnly(s.listUsers)},
		{"POST /api/users", "/api/users", s.adminOnly(s.createUser)},
		{"PUT /api/users/{id}", "/api/users/{id}", s.adminOnly(s.updateUser)},
		{"DELETE /api/users/{id}", "/api/users/{id}", s.adminOnly(s.deleteUser)},
	}
}

// refreshUserFeeds: manual and immediate refresh of ALL the feeds of the
// authenticated user (the spec /feeds/update route is admin only).
func (s *Server) refreshUserFeeds(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	feeds, err := s.store.ListFeeds(u.ID)
	if err != nil {
		s.logError(w, r, "list feeds for refresh", err)
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	inserted := int64(0)
	for i := range feeds {
		f := feeds[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if res := s.refresher.Refresh(r.Context(), &f); res.Err == nil {
				atomic.AddInt64(&inserted, res.Inserted)
			}
		}()
	}
	wg.Wait()
	s.log.Info("manual refresh", "user", u.Username, "feeds", len(feeds), "new", inserted)
	writeJSON(w, http.StatusOK, map[string]any{"refreshed": len(feeds), "newItems": inserted})
}

// NOTE (SPEC §4.2): the OCS stub of /ocs/v2.php/cloud/user that this
// package served for news-android was removed in H5, the route is registered ONLY by the
// notes module, whose extended handler emits the same JSON payload
// (id=username, displayname, display-name, 401 without auth) when the client
// asks for ?format=json or Accept: application/json.

var validUsername = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{2,31}$`)

type meResponse struct {
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
	Language    string `json:"language"`
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	writeJSON(w, http.StatusOK, meResponse{u.Username, u.DisplayName, u.Role, u.Language})
}

func (s *Server) updateMe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DisplayName *string `json:"displayName"`
		Language    *string `json:"language"`
	}
	if err := decodeBody(r, &body); err != nil {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_body")
		return
	}
	u := user(r)
	name, lang := u.DisplayName, u.Language
	if body.DisplayName != nil {
		name = *body.DisplayName
	}
	if body.Language != nil {
		if !validLanguage(*body.Language) {
			errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_language")
			return
		}
		lang = *body.Language
	}
	if err := s.store.UpdateProfile(u.ID, name, lang); err != nil {
		s.logError(w, r, "update profile", err)
		return
	}
	writeJSON(w, http.StatusOK, meResponse{u.Username, name, u.Role, lang})
}

func (s *Server) changeMyPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Current string `json:"currentPassword"`
		New     string `json:"newPassword"`
	}
	if err := decodeBody(r, &body); err != nil {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_body")
		return
	}
	u := user(r)
	// the current password must validate (bcrypt against the stored hash)
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(body.Current)) != nil {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_password")
		return
	}
	if !validPassword(body.New) {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_password")
		return
	}
	hash, err := auth.HashPassword(body.New)
	if err != nil {
		s.logError(w, r, "hash password", err)
		return
	}
	if err := s.store.UpdatePassword(u.ID, hash); err != nil {
		s.logError(w, r, "change password", err)
		return
	}
	writeEmpty(w)
}

// userSettings: app preferences persisted per user. The source of
// truth for the language is still users.language, the rest goes in user_settings.
type userSettings struct {
	Theme           string `json:"theme"`           // system|light|dark
	ReaderMaxWidth  string `json:"readerMaxWidth"`  // narrow|normal|wide
	FeedIntervalMin string `json:"feedIntervalMin"` // minutes, empty = global default
	ReaderFont      string `json:"readerFont"`      // default|serif|sans|mono
	ReaderFontSize  string `json:"readerFontSize"`  // px, empty = 15
	NtfyTopic       string `json:"ntfyTopic"`       // user ntfy topic, empty = global/off
}

func (s *Server) mySettings(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	theme, _ := s.store.GetUserSetting(u.ID, "theme")
	width, _ := s.store.GetUserSetting(u.ID, "reader_max_width")
	interval, _ := s.store.GetUserSetting(u.ID, "feed_interval_min")
	font, _ := s.store.GetUserSetting(u.ID, "reader_font")
	fontSize, _ := s.store.GetUserSetting(u.ID, "reader_font_size")
	ntfy, _ := s.store.GetUserSetting(u.ID, "ntfy_topic")
	if theme == "" {
		theme = "system"
	}
	if width == "" {
		width = "normal"
	}
	if font == "" {
		font = "default"
	}
	if fontSize == "" {
		fontSize = "15"
	}
	writeJSON(w, http.StatusOK, userSettings{theme, width, interval, font, fontSize, ntfy})
}

func (s *Server) updateMySettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Theme           *string `json:"theme"`
		ReaderMaxWidth  *string `json:"readerMaxWidth"`
		FeedIntervalMin *string `json:"feedIntervalMin"`
		ReaderFont      *string `json:"readerFont"`
		ReaderFontSize  *string `json:"readerFontSize"`
		NtfyTopic       *string `json:"ntfyTopic"`
	}
	if err := decodeBody(r, &body); err != nil {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_body")
		return
	}
	u := user(r)
	if body.Theme != nil {
		if !validTheme(*body.Theme) {
			errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_theme")
			return
		}
		v := *body.Theme
		if v == "system" {
			v = ""
		}
		if err := s.store.SetUserSetting(u.ID, "theme", v); err != nil {
			s.logError(w, r, "store theme", err)
			return
		}
	}
	if body.ReaderMaxWidth != nil {
		if !validWidth(*body.ReaderMaxWidth) {
			errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_reader_width")
			return
		}
		v := *body.ReaderMaxWidth
		if v == "normal" {
			v = ""
		}
		if err := s.store.SetUserSetting(u.ID, "reader_max_width", v); err != nil {
			s.logError(w, r, "store reader width", err)
			return
		}
	}
	if body.FeedIntervalMin != nil {
		v := *body.FeedIntervalMin
		if v != "" {
			if !validInterval(v) {
				errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_feed_interval")
				return
			}
		}
		if err := s.store.SetUserSetting(u.ID, "feed_interval_min", v); err != nil {
			s.logError(w, r, "store interval", err)
			return
		}
	}
	if body.ReaderFont != nil {
		if !validFont(*body.ReaderFont) {
			errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_reader_font")
			return
		}
		v := *body.ReaderFont
		if v == "default" {
			v = ""
		}
		if err := s.store.SetUserSetting(u.ID, "reader_font", v); err != nil {
			s.logError(w, r, "store reader font", err)
			return
		}
	}
	if body.ReaderFontSize != nil {
		v := *body.ReaderFontSize
		if v != "" && !validFontSize(v) {
			errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_reader_font_size")
			return
		}
		if v == "15" {
			v = ""
		}
		if err := s.store.SetUserSetting(u.ID, "reader_font_size", v); err != nil {
			s.logError(w, r, "store font size", err)
			return
		}
	}
	if body.NtfyTopic != nil {
		v := strings.TrimSpace(*body.NtfyTopic)
		if v != "" && !validNtfyTopic(v) {
			errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_ntfy_topic")
			return
		}
		if err := s.store.SetUserSetting(u.ID, "ntfy_topic", v); err != nil {
			s.logError(w, r, "store ntfy topic", err)
			return
		}
	}
	s.mySettings(w, r)
}

func validTheme(t string) bool { return t == "system" || t == "light" || t == "dark" }
func validWidth(w string) bool { return w == "narrow" || w == "normal" || w == "wide" }
func validInterval(v string) bool {
	n, err := strconv.Atoi(v)
	return err == nil && n >= 5 && n <= 1440
}
func validFont(f string) bool {
	return f == "default" || f == "serif" || f == "sans" || f == "mono"
}
func validFontSize(v string) bool {
	n, err := strconv.Atoi(v)
	return err == nil && n >= 13 && n <= 20
}

// validNtfyTopic: ntfy topic = [a-zA-Z0-9_-], up to 64 chars.
func validNtfyTopic(v string) bool {
	if len(v) > 64 {
		return false
	}
	for _, r := range v {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers()
	if err != nil {
		s.logError(w, r, "list users", err)
		return
	}
	out := make([]meResponse, 0, len(users))
	for _, u := range users {
		out = append(out, meResponse{u.Username, u.DisplayName, u.Role, u.Language})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username    string  `json:"username"`
		Password    string  `json:"password"`
		DisplayName string  `json:"displayName"`
		Role        string  `json:"role"`
		Language    *string `json:"language"`
	}
	if err := decodeBody(r, &body); err != nil {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_body")
		return
	}
	if !validUsername.MatchString(body.Username) {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_username")
		return
	}
	if !validPassword(body.Password) {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_password")
		return
	}
	role := "user"
	if body.Role != "" {
		if !validRole(body.Role) {
			errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_role")
			return
		}
		role = body.Role
	}
	language := "auto"
	if body.Language != nil {
		if !validLanguage(*body.Language) {
			errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_language")
			return
		}
		language = *body.Language
	}
	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		s.logError(w, r, "hash password", err)
		return
	}
	id, err := s.store.CreateUser(body.Username, hash, body.DisplayName, role)
	if err != nil {
		errorStatus(w, r, http.StatusConflict, "user_exists")
		return
	}
	if language != "auto" {
		s.store.SetUserLanguage(id, language)
	}
	created, err := s.store.GetUserByID(id)
	if err != nil {
		s.logError(w, r, "read created user", err)
		return
	}
	writeJSON(w, http.StatusCreated, meResponse{created.Username, created.DisplayName, created.Role, created.Language})
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		errorStatus(w, r, http.StatusNotFound, "user_not_found")
		return
	}
	target, err := s.store.GetUserByID(id)
	if errors.Is(err, store.ErrNotFound) {
		errorStatus(w, r, http.StatusNotFound, "user_not_found")
		return
	}
	if err != nil {
		s.logError(w, r, "read user", err)
		return
	}
	var body struct {
		Role     *string `json:"role"`
		Language *string `json:"language"`
		Password string  `json:"password"`
	}
	if err := decodeBody(r, &body); err != nil {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_body")
		return
	}
	if body.Role != nil && !validRole(*body.Role) {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_role")
		return
	}
	if body.Language != nil && !validLanguage(*body.Language) {
		errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_language")
		return
	}
	// guard: demoting the last admin leaves the system closed
	if body.Role != nil && *body.Role == "user" && target.Role == "admin" {
		if admins, err := s.store.CountAdmins(); err == nil && admins <= 1 {
			errorStatus(w, r, http.StatusConflict, "cannot_delete_last_admin")
			return
		}
	}
	if body.Password != "" {
		if !validPassword(body.Password) {
			errorStatus(w, r, http.StatusUnprocessableEntity, "invalid_password")
			return
		}
		hash, err := auth.HashPassword(body.Password)
		if err != nil {
			s.logError(w, r, "hash password", err)
			return
		}
		if err := s.store.UpdatePassword(id, hash); err != nil {
			s.logError(w, r, "change password", err)
			return
		}
	}
	if err := s.store.UpdateUserFields(id, body.Role, body.Language); err != nil {
		s.logError(w, r, "update user", err)
		return
	}
	updated, _ := s.store.GetUserByID(id)
	writeJSON(w, http.StatusOK, meResponse{updated.Username, updated.DisplayName, updated.Role, updated.Language})
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		errorStatus(w, r, http.StatusNotFound, "user_not_found")
		return
	}
	if id == user(r).ID {
		errorStatus(w, r, http.StatusConflict, "cannot_delete_self")
		return
	}
	switch err := s.store.DeleteUser(id); {
	case errors.Is(err, store.ErrNotFound):
		errorStatus(w, r, http.StatusNotFound, "user_not_found")
	case errors.Is(err, store.ErrConflict):
		errorStatus(w, r, http.StatusConflict, "cannot_delete_last_admin")
	case err != nil:
		s.logError(w, r, "delete user", err)
	default:
		writeEmpty(w)
	}
}

func validLanguage(l string) bool  { return l == "auto" || l == "es" || l == "en" }
func validRole(role string) bool   { return role == "user" || role == "admin" }
func validPassword(pw string) bool { return len(pw) >= 8 }
