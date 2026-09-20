package store

import "sort"

// Sender is a known invoice sender. Mail from an address that is not listed
// here is ignored by the mail reader.
type Sender struct {
	ID     int64
	Email  string
	Name   string
	SKR04  string
	Active bool
}

// Settings returns all configuration values as a flat key/value map.
func (db *DB) Settings() (map[string]string, error) {
	rows, err := db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

// SetSetting stores one configuration value.
func (db *DB) SetSetting(key, value string) error {
	_, err := db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// SetSettings stores many configuration values in one transaction.
func (db *DB) SetSettings(values map[string]string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for k, v := range values {
		if _, err := stmt.Exec(k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Senders lists the known senders, ordered by mail address.
func (db *DB) Senders() ([]Sender, error) {
	rows, err := db.Query(`SELECT id, email, name, skr04, active FROM senders ORDER BY email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sender
	for rows.Next() {
		var s Sender
		if err := rows.Scan(&s.ID, &s.Email, &s.Name, &s.SKR04, &s.Active); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out, nil
}

// SaveSender inserts or updates a known sender, keyed by mail address.
func (db *DB) SaveSender(s Sender) error {
	_, err := db.Exec(`INSERT INTO senders (email, name, skr04, active) VALUES (?, ?, ?, ?)
		ON CONFLICT(email) DO UPDATE SET name=excluded.name, skr04=excluded.skr04, active=excluded.active`,
		s.Email, s.Name, s.SKR04, s.Active)
	return err
}

// DeleteSender removes a known sender.
func (db *DB) DeleteSender(id int64) error {
	_, err := db.Exec(`DELETE FROM senders WHERE id=?`, id)
	return err
}
