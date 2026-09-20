package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func init() {
	// The delay only exists to slow down guessing; tests should not wait.
	wrongPasswordDelay = 0
}

// TestUnprotectedByDefault covers the deliberate case: no password configured,
// everything reachable, and the pages say so.
func TestUnprotectedByDefault(t *testing.T) {
	h, _ := newServer(t)
	rec := get(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200 when no password is set", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Kein Passwort gesetzt") {
		t.Error("the dashboard does not warn about the missing password")
	}
}

func TestGuardRedirectsWhenProtected(t *testing.T) {
	h, db := newServer(t)
	if err := db.SetPassword("korrekt-pferd-batterie"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/settings", "/partials/status"} {
		rec := get(t, h, path)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
			t.Errorf("GET %s = %d to %q, want a redirect to /login",
				path, rec.Code, rec.Header().Get("Location"))
		}
	}
	// The login page and the assets stay reachable, or nobody could log in.
	for _, path := range []string{"/login", "/static/htmx.min.js"} {
		if rec := get(t, h, path); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
	}
}

// TestGuardTellsHtmxToNavigate: an htmx request that gets a redirect would
// swap the login form into the page, so the guard sends HX-Redirect instead.
func TestGuardTellsHtmxToNavigate(t *testing.T) {
	h, db := newServer(t)
	if err := db.SetPassword("korrekt-pferd-batterie"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/partials/status", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("HX-Redirect"); got != "/login" {
		t.Errorf("HX-Redirect = %q, want /login", got)
	}
}

func TestLoginLogout(t *testing.T) {
	h, db := newServer(t)
	if err := db.SetPassword("korrekt-pferd-batterie"); err != nil {
		t.Fatal(err)
	}

	// A wrong password gets the form back, with no cookie.
	rec := post(t, h, "/login", url.Values{"password": {"falsch"}})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong password = %d, want 401", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("a wrong password handed out a cookie")
	}

	rec = post(t, h, "/login", url.Values{"password": {"korrekt-pferd-batterie"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login = %d\n%s", rec.Code, rec.Body.String())
	}
	cookie := sessionFrom(t, rec)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie = %+v, want HttpOnly and SameSite=Lax", cookie)
	}

	// With the cookie the dashboard is reachable again.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / with a session = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Abmelden") {
		t.Error("no logout button for a logged-in user")
	}

	// Logging out invalidates it.
	req = httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("logout = %d", rec.Code)
	}
	if ok, _ := db.ValidSession(cookie.Value); ok {
		t.Error("the session survived logging out")
	}
}

func TestLoginReturnsToTheRequestedPage(t *testing.T) {
	h, db := newServer(t)
	if err := db.SetPassword("korrekt-pferd-batterie"); err != nil {
		t.Fatal(err)
	}
	rec := post(t, h, "/login", url.Values{
		"password": {"korrekt-pferd-batterie"}, "next": {"/settings"},
	})
	if got := rec.Header().Get("Location"); got != "/settings" {
		t.Errorf("redirected to %q, want /settings", got)
	}
}

// TestLoginWillNotRedirectOffSite: "next" comes from the URL, so it must never
// send the browser somewhere else.
func TestLoginWillNotRedirectOffSite(t *testing.T) {
	for _, next := range []string{"https://example.com/", "//example.com/", "javascript:alert(1)"} {
		if got := safeNext(next); got != "/" {
			t.Errorf("safeNext(%q) = %q, want /", next, got)
		}
	}
	if got := safeNext("/invoices/7"); got != "/invoices/7" {
		t.Errorf("safeNext dropped a local path: %q", got)
	}
}

func TestSetPassword(t *testing.T) {
	h, db := newServer(t)

	if rec := post(t, h, "/password", url.Values{
		"password": {"langes-passwort"}, "repeat": {"anderes-passwort"},
	}); !strings.Contains(rec.Body.String(), "stimmen nicht überein") {
		t.Error("mismatched passwords were accepted")
	}
	if set, _ := db.PasswordSet(); set {
		t.Fatal("a password was set despite the mismatch")
	}

	if rec := post(t, h, "/password", url.Values{
		"password": {"kurz"}, "repeat": {"kurz"},
	}); !strings.Contains(rec.Body.String(), "8 Zeichen") {
		t.Error("a too short password was accepted")
	}

	rec := post(t, h, "/password", url.Values{
		"password": {"langes-passwort"}, "repeat": {"langes-passwort"},
	})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Errorf("setting a password = %d to %q, want a redirect to /login",
			rec.Code, rec.Header().Get("Location"))
	}
	if ok, err := db.CheckPassword("langes-passwort"); err != nil || !ok {
		t.Errorf("the new password does not work: %v, %v", ok, err)
	}
}

func TestSetPasswordCanSwitchProtectionOff(t *testing.T) {
	h, db := newServer(t)
	if err := db.SetPassword("korrekt-pferd-batterie"); err != nil {
		t.Fatal(err)
	}
	token, err := db.CreateSession(sessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/password",
		strings.NewReader(url.Values{"password": {""}, "repeat": {""}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ungeschützt") {
		t.Errorf("switching protection off = %d\n%s", rec.Code, rec.Body.String())
	}
	if set, _ := db.PasswordSet(); set {
		t.Error("protection is still on")
	}
}

func sessionFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("no session cookie in the response")
	return nil
}
