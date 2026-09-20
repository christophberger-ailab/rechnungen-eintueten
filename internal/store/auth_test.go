package store

import (
	"testing"
	"time"
)

func TestPassword(t *testing.T) {
	db := open(t)

	if set, err := db.PasswordSet(); err != nil || set {
		t.Fatalf("PasswordSet on a fresh database = %v, %v", set, err)
	}
	// With no password configured nothing matches, not everything.
	if ok, err := db.CheckPassword("irgendwas"); err != nil || ok {
		t.Errorf("CheckPassword without a password = %v, %v", ok, err)
	}

	if err := db.SetPassword("korrekt-pferd-batterie"); err != nil {
		t.Fatal(err)
	}
	if set, err := db.PasswordSet(); err != nil || !set {
		t.Fatalf("PasswordSet after setting one = %v, %v", set, err)
	}
	if ok, err := db.CheckPassword("korrekt-pferd-batterie"); err != nil || !ok {
		t.Errorf("the right password was rejected: %v, %v", ok, err)
	}
	if ok, err := db.CheckPassword("falsch"); err != nil || ok {
		t.Errorf("a wrong password was accepted: %v, %v", ok, err)
	}

	// The hash is what is stored, never the password itself.
	values, err := db.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if values[passwordKey] == "korrekt-pferd-batterie" || values[passwordKey] == "" {
		t.Errorf("stored value = %q, want a hash", values[passwordKey])
	}

	if err := db.SetPassword(""); err != nil {
		t.Fatal(err)
	}
	if set, _ := db.PasswordSet(); set {
		t.Error("an empty password must switch protection off")
	}
}

func TestSessions(t *testing.T) {
	db := open(t)

	token, err := db.CreateSession(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("CreateSession returned an empty token")
	}
	if ok, err := db.ValidSession(token); err != nil || !ok {
		t.Errorf("fresh session = %v, %v", ok, err)
	}
	if ok, _ := db.ValidSession("erfunden"); ok {
		t.Error("an invented token was accepted")
	}
	if ok, _ := db.ValidSession(""); ok {
		t.Error("an empty token was accepted")
	}

	// Only the hash is stored, so a leaked database hands out no sessions.
	var stored string
	if err := db.QueryRow(`SELECT token_hash FROM sessions`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == token {
		t.Error("the session token itself is stored, not its hash")
	}

	if err := db.DeleteSession(token); err != nil {
		t.Fatal(err)
	}
	if ok, _ := db.ValidSession(token); ok {
		t.Error("session survived being deleted")
	}
}

func TestSessionsExpire(t *testing.T) {
	db := open(t)
	token, err := db.CreateSession(-time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ValidSession(token); err != nil || ok {
		t.Errorf("expired session = %v, %v", ok, err)
	}
	var left int
	if err := db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d expired sessions left behind", left)
	}
}

func TestChangingThePasswordEndsSessions(t *testing.T) {
	db := open(t)
	if err := db.SetPassword("erstes-passwort"); err != nil {
		t.Fatal(err)
	}
	token, err := db.CreateSession(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetPassword("zweites-passwort"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := db.ValidSession(token); ok {
		t.Error("a session survived the password change")
	}
}
