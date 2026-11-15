package api

import (
	"fmt"
	"strings"
	"testing"
)

func TestUsersEndpoints(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})

	// me + profile update
	code, body := e.do(t, "GET", "/api/me", e.user, e.pass, nil)
	if code != 200 || !strings.Contains(string(body), `"role":"admin"`) {
		t.Fatalf("me: %d %s", code, body)
	}
	code, body = e.do(t, "PUT", "/api/me", e.user, e.pass,
		map[string]string{"language": "en", "displayName": "Nacho G"})
	if code != 200 || !strings.Contains(string(body), `"language":"en"`) {
		t.Fatalf("update me: %d %s", code, body)
	}
	// invalid language -> 422 with a stable code
	code, body = e.do(t, "PUT", "/api/me", e.user, e.pass, map[string]string{"language": "fr"})
	if code != 422 || !strings.Contains(string(body), `"code":"invalid_language"`) {
		t.Fatalf("invalid language: %d %s", code, body)
	}
	// restore es so as not to affect TestErrorI18n (DB per test, but just in case)
	e.do(t, "PUT", "/api/me", e.user, e.pass, map[string]string{"language": "es"})

	// password change: wrong current -> 422, right -> 204 and login with the new one
	code, _ = e.do(t, "PUT", "/api/me/password", e.user, e.pass,
		map[string]string{"currentPassword": "badpass", "newPassword": "newpass1234"})
	if code != 422 {
		t.Fatalf("password actual mal: %d", code)
	}
	if code, _ = e.do(t, "PUT", "/api/me/password", e.user, e.pass,
		map[string]string{"currentPassword": "pass1234", "newPassword": "newpass1234"}); code != 204 {
		t.Fatalf("cambio password: %d", code)
	}
	if code, _ = e.do(t, "GET", "/api/me", e.user, "pass1234", nil); code != 401 {
		t.Fatalf("old password must fail: %d", code)
	}
	if code, _ = e.do(t, "GET", "/api/me", e.user, "newpass1234", nil); code != 200 {
		t.Fatalf("new password must be valid: %d", code)
	}
	// short -> 422
	if code, _ = e.do(t, "PUT", "/api/me/password", e.user, "newpass1234",
		map[string]string{"currentPassword": "newpass1234", "newPassword": "short"}); code != 422 {
		t.Fatalf("password corta: %d", code)
	}

	// admin creates a user, duplicate -> 409, invalid username -> 422
	code, body = e.do(t, "POST", "/api/users", e.user, "newpass1234",
		map[string]string{"username": "maria", "password": "maria1234", "displayName": "María", "role": "user"})
	if code != 201 || !strings.Contains(string(body), `"username":"maria"`) {
		t.Fatalf("create user: %d %s", code, body)
	}
	if code, _ = e.do(t, "POST", "/api/users", e.user, "newpass1234",
		map[string]string{"username": "maria", "password": "maria1234"}); code != 409 {
		t.Fatalf("duplicate: %d", code)
	}
	if code, _ = e.do(t, "POST", "/api/users", e.user, "newpass1234",
		map[string]string{"username": "M", "password": "maria1234"}); code != 422 {
		t.Fatalf("invalid username: %d", code)
	}

	// non-admin -> 401
	if code, _ = e.do(t, "GET", "/api/users", "other", "other1234", nil); code != 401 {
		t.Fatalf("users by non-admin: %d", code)
	}
	// maria cannot manage users but she can use her profile
	if code, _ = e.do(t, "GET", "/api/me", "maria", "maria1234", nil); code != 200 {
		t.Fatalf("maria me: %d", code)
	}

	// list
	code, body = e.do(t, "GET", "/api/users", e.user, "newpass1234", nil)
	if code != 200 || !strings.Contains(string(body), "maria") || !strings.Contains(string(body), "nacho") {
		t.Fatalf("list users: %d %s", code, body)
	}

	// update maria role to admin and back, last admin protected
	var mariaID int64 = 3 // nacho=1, other=2, maria=3 in a fresh DB per test
	code, _ = e.do(t, "PUT", fmt.Sprintf("/api/users/%d", mariaID), e.user, "newpass1234",
		map[string]string{"role": "admin"})
	if code != 200 {
		t.Fatalf("promover maria: %d", code)
	}
	code, body = e.do(t, "PUT", fmt.Sprintf("/api/users/%d", mariaID), e.user, "newpass1234",
		map[string]string{"role": "user"})
	if code != 200 {
		t.Fatalf("demote maria (there are 2 admins): %d %s", code, body)
	}
	// now only nacho is left as admin -> demote himself -> 409
	code, _ = e.do(t, "PUT", "/api/users/1", e.user, "newpass1234", map[string]string{"role": "user"})
	if code != 409 {
		t.Fatalf("demote last admin: %d", code)
	}
	// delete himself -> 409
	if code, _ = e.do(t, "DELETE", "/api/users/1", e.user, "newpass1234", nil); code != 409 {
		t.Fatalf("self delete: %d", code)
	}
	// delete maria -> 204 and her content disappears
	if code, _ = e.do(t, "DELETE", fmt.Sprintf("/api/users/%d", mariaID), e.user, "newpass1234", nil); code != 204 {
		t.Fatalf("delete maria: %d", code)
	}
	if code, _ = e.do(t, "GET", "/api/me", "maria", "maria1234", nil); code != 401 {
		t.Fatalf("deleted maria must not authenticate: %d", code)
	}
}

func TestRefreshEndpoint(t *testing.T) {
	ff := &fakeFetcher{}
	e := newTestEnv(t, ff)
	e.do(t, "POST", "/feeds", e.user, e.pass, map[string]string{"url": "https://r.example/f"})
	before := ff.fetches
	code, body := e.do(t, "POST", "/refresh", e.user, e.pass, nil)
	if code != 200 || !strings.Contains(string(body), `"refreshed":1`) {
		t.Fatalf("refresh: %d %s", code, body)
	}
	if ff.fetches != before+1 {
		t.Fatalf("expected 1 new fetch, I have %d", ff.fetches-before)
	}
}
