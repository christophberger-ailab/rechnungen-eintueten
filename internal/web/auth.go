package web

import (
	"net"
	"net/http"
	"strings"
	"time"
)

// sessionCookie is the cookie holding the login token.
const sessionCookie = "rechnungen_session"

// sessionTTL is how long a login lasts. The application sits behind a private
// network, so the session may be generous.
const sessionTTL = 30 * 24 * time.Hour

// wrongPasswordDelay slows down guessing. bcrypt already makes each attempt
// expensive; this makes a script pointless.
var wrongPasswordDelay = time.Second

// guard requires a valid session for everything except the login page and the
// static files.
//
// When no password is configured, access control is off and every request goes
// through - the tool is meant to run inside a private network, where that can
// be a deliberate choice. The dashboard says so in that case.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" || strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		protected, err := s.db.PasswordSet()
		if err != nil {
			s.fail(w, err)
			return
		}
		if !protected || s.loggedIn(r) {
			next.ServeHTTP(w, r)
			return
		}
		// htmx swaps the response into the page, so a redirect would land the
		// login form inside the dashboard. Tell it to navigate instead.
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

func (s *Server) loggedIn(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	ok, err := s.db.ValidSession(cookie.Value)
	if err != nil {
		s.db.Log(0, 0, "web", "error", "Sitzung prüfen: %v", err)
		return false
	}
	return ok
}

// login shows the password form.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	s.renderLogin(w, r, "")
}

// doLogin checks the password and starts a session.
func (s *Server) doLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.fail(w, err)
		return
	}
	ok, err := s.db.CheckPassword(r.PostFormValue("password"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if !ok {
		time.Sleep(wrongPasswordDelay)
		s.db.Log(0, 0, "web", "warn", "Fehlgeschlagener Anmeldeversuch von %s", clientIP(r))
		s.renderLogin(w, r, "Falsches Passwort.")
		return
	}
	token, err := s.db.CreateSession(sessionTTL)
	if err != nil {
		s.fail(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   overTLS(r),
		Expires:  time.Now().Add(sessionTTL),
	})
	http.Redirect(w, r, safeNext(r.PostFormValue("next")), http.StatusSeeOther)
}

// logout ends the session and clears the cookie.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if err := s.db.DeleteSession(cookie.Value); err != nil {
			s.db.Log(0, 0, "web", "error", "Abmelden: %v", err)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: overTLS(r), MaxAge: -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// setPassword changes the password from the settings page. Changing it ends
// every session, including this one, so the browser is sent back to the login
// form.
func (s *Server) setPassword(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.fail(w, err)
		return
	}
	password, repeat := r.PostFormValue("password"), r.PostFormValue("repeat")
	switch {
	case password != repeat:
		s.renderSettings(w, r, "", "Die beiden Passwörter stimmen nicht überein.")
		return
	case password != "" && len(password) < 8:
		s.renderSettings(w, r, "", "Das Passwort braucht mindestens 8 Zeichen.")
		return
	}
	if err := s.db.SetPassword(password); err != nil {
		s.fail(w, err)
		return
	}
	if password == "" {
		s.db.Log(0, 0, "web", "warn", "Passwortschutz wurde abgeschaltet")
		s.renderSettings(w, r, "Passwortschutz abgeschaltet. Das Web-UI ist jetzt ungeschützt.", "")
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if errMsg != "" {
		w.WriteHeader(http.StatusUnauthorized)
	}
	v := &view{Title: "Anmelden", Active: "login", Error: errMsg,
		Next: safeNext(r.URL.Query().Get("next"))}
	if err := s.pages["login"].ExecuteTemplate(w, "layout.html", v); err != nil {
		s.db.Log(0, 0, "web", "error", "Anmeldeseite: %v", err)
	}
}

// safeNext keeps the post-login redirect on this site: only a path, never a
// URL pointing somewhere else.
func safeNext(next string) string {
	if strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") {
		return next
	}
	return "/"
}

func overTLS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
