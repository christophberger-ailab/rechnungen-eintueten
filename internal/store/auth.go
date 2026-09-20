package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// passwordKey is the settings entry holding the bcrypt hash of the single
// user's password. There is one user, so there is no user table.
const passwordKey = "auth.password_hash"

// SetPassword stores the password. An empty password removes it, which turns
// access control off - the application is meant to run inside a private
// network where that can be a deliberate choice.
func (db *DB) SetPassword(plain string) error {
	if plain == "" {
		_, err := db.Exec(`DELETE FROM settings WHERE key=?`, passwordKey)
		if err == nil {
			err = db.DeleteAllSessions()
		}
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if err := db.SetSetting(passwordKey, string(hash)); err != nil {
		return err
	}
	// Whoever was logged in with the old password has to log in again.
	return db.DeleteAllSessions()
}

// PasswordSet reports whether a password is configured, i.e. whether the web
// UI asks for one.
func (db *DB) PasswordSet() (bool, error) {
	hash, err := db.passwordHash()
	return hash != "", err
}

// CheckPassword reports whether plain is the configured password. It returns
// false when no password is set: callers decide what an unprotected
// installation means, and "everything matches" is never the safe reading.
func (db *DB) CheckPassword(plain string) (bool, error) {
	hash, err := db.passwordHash()
	if err != nil || hash == "" {
		return false, err
	}
	err = bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain))
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return false, nil
	}
	return err == nil, err
}

func (db *DB) passwordHash() (string, error) {
	var hash string
	err := db.QueryRow(`SELECT value FROM settings WHERE key=?`, passwordKey).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return hash, err
}

// CreateSession starts a login session and returns the token for the cookie.
// Only a hash of the token is stored, so a copy of the database does not hand
// anyone a valid session.
func (db *DB) CreateSession(ttl time.Duration) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	_, err := db.Exec(`INSERT INTO sessions (token_hash, expires_at) VALUES (?, ?)`,
		hashToken(token), time.Now().Add(ttl).UTC().Format(time.RFC3339))
	if err != nil {
		return "", err
	}
	return token, nil
}

// ValidSession reports whether the token belongs to a session that has not
// expired. Expired rows are cleaned up as they are encountered.
func (db *DB) ValidSession(token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	if _, err := db.Exec(`DELETE FROM sessions WHERE expires_at < ?`,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		return false, err
	}
	var one int
	err := db.QueryRow(`SELECT 1 FROM sessions WHERE token_hash=?`, hashToken(token)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// DeleteSession ends one session, which is what logging out does.
func (db *DB) DeleteSession(token string) error {
	_, err := db.Exec(`DELETE FROM sessions WHERE token_hash=?`, hashToken(token))
	return err
}

// DeleteAllSessions ends every session.
func (db *DB) DeleteAllSessions() error {
	_, err := db.Exec(`DELETE FROM sessions`)
	return err
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
