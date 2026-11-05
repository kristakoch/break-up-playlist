// Package sqlite implements atomic local persistence behind the application store interface.
package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/kristakoch/break-up-playlist/internal/domain"
	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
	mu sync.RWMutex
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if _, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;`); err == nil {
		err = s.migrate()
	}

	if err != nil {
		db.Close()

		return nil, err
	}

	if err = os.Chmod(path, 0600); err != nil {
		db.Close()

		return nil, err
	}

	return s, nil
}

// Version two moves songs out of the original collection documents, preserving all saved state.
func (s *Store) migrate() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}

	defer tx.Rollback()

	if _, err = tx.Exec(`CREATE TABLE IF NOT EXISTS schema_versions(version INTEGER PRIMARY KEY);
 CREATE TABLE IF NOT EXISTS collections(id TEXT PRIMARY KEY, document BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS songs(collection_id TEXT NOT NULL REFERENCES collections(id) ON DELETE CASCADE, id TEXT NOT NULL, position INTEGER NOT NULL, document BLOB NOT NULL, PRIMARY KEY(collection_id,id));
 CREATE INDEX IF NOT EXISTS songs_order ON songs(collection_id,position);`); err != nil {
		return err
	}

	var version int

	if err = tx.QueryRow("SELECT COALESCE(MAX(version),0) FROM schema_versions").Scan(&version); err != nil {
		return err
	}

	if version > 2 {
		return errors.New("database was created by a newer application; use that version")
	}

	if version < 2 {
		rows, err := tx.Query("SELECT document FROM collections")
		if err != nil {
			return err
		}

		var existing []domain.Collection

		for rows.Next() {
			var b []byte

			if err = rows.Scan(&b); err != nil {
				rows.Close()

				return err
			}

			var c domain.Collection

			if err = json.Unmarshal(b, &c); err != nil {
				rows.Close()

				return err
			}

			existing = append(existing, c)
		}

		if err = rows.Err(); err != nil {
			rows.Close()

			return err
		}

		rows.Close()

		for _, c := range existing {
			for _, song := range c.Songs {
				b, err := json.Marshal(song)
				if err != nil {
					return err
				}

				if _, err = tx.Exec("INSERT OR REPLACE INTO songs(collection_id,id,position,document) VALUES(?,?,?,?)", c.ID, song.ID, song.Position, b); err != nil {
					return err
				}
			}

			c.Songs = nil
			b, err := json.Marshal(c)
			if err != nil {
				return err
			}

			if _, err = tx.Exec("UPDATE collections SET document=? WHERE id=?", b, c.ID); err != nil {
				return err
			}
		}

		if _, err = tx.Exec("INSERT INTO schema_versions(version) VALUES(2)"); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *Store) Close() error { return s.db.Close() }

// List returns complete collections through the small application store interface.
func (s *Store) List(ctx context.Context) ([]domain.Collection, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.QueryContext(ctx, "SELECT document FROM collections ORDER BY id")
	if err != nil {
		return nil, err
	}

	var out []domain.Collection

	for rows.Next() {
		var b []byte

		if err = rows.Scan(&b); err != nil {
			rows.Close()

			return nil, err
		}

		var c domain.Collection

		if err = json.Unmarshal(b, &c); err != nil {
			rows.Close()

			return nil, err
		}

		out = append(out, c)
	}

	err = rows.Err()
	rows.Close()

	if err != nil {
		return nil, err
	}
	// The small application interface returns complete collections; all reads remain local.
	for i := range out {
		out[i].Songs, err = s.songs(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
	}

	return out, nil
}

func (s *Store) songs(ctx context.Context, id string) ([]domain.Song, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT document FROM songs WHERE collection_id=? ORDER BY position,id", id)
	if err != nil {
		return nil, err
	}

	defer rows.Close()
	var songs []domain.Song

	for rows.Next() {
		var b []byte

		if err = rows.Scan(&b); err != nil {
			return nil, err
		}

		var song domain.Song

		if err = json.Unmarshal(b, &song); err != nil {
			return nil, err
		}

		songs = append(songs, song)
	}

	return songs, rows.Err()
}

func (s *Store) Get(ctx context.Context, id string) (domain.Collection, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.get(ctx, id)
}

func (s *Store) get(ctx context.Context, id string) (domain.Collection, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, "SELECT document FROM collections WHERE id=?", id).Scan(&b)
	var c domain.Collection

	if err == nil {
		err = json.Unmarshal(b, &c)
	}

	if err == nil {
		c.Songs, err = s.songs(ctx, id)
	}

	return c, err
}

func (s *Store) Save(ctx context.Context, c domain.Collection) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.save(ctx, c)
}

func (s *Store) save(ctx context.Context, c domain.Collection) error {
	songs := c.Songs
	c.Songs = nil
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	defer tx.Rollback()

	if _, err = tx.ExecContext(ctx, "INSERT INTO collections(id,document) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET document=excluded.document WHERE document!=excluded.document", c.ID, b); err != nil {
		return err
	}

	rows, err := tx.QueryContext(ctx, "SELECT id,document FROM songs WHERE collection_id=?", c.ID)
	if err != nil {
		return err
	}

	existing := map[string][]byte{}

	for rows.Next() {
		var id string
		var doc []byte

		if err = rows.Scan(&id, &doc); err != nil {
			rows.Close()

			return err
		}

		existing[id] = doc
	}

	if err = rows.Err(); err != nil {
		rows.Close()

		return err
	}

	rows.Close()
	seen := map[string]bool{}

	for _, song := range songs {
		if song.ID == "" || seen[song.ID] {
			return errors.New("songs must have unique nonempty IDs")
		}

		seen[song.ID] = true
		doc, err := json.Marshal(song)
		if err != nil {
			return err
		}

		if !bytes.Equal(existing[song.ID], doc) {
			if _, err = tx.ExecContext(ctx, "INSERT INTO songs(collection_id,id,position,document) VALUES(?,?,?,?) ON CONFLICT(collection_id,id) DO UPDATE SET position=excluded.position,document=excluded.document", c.ID, song.ID, song.Position, doc); err != nil {
				return err
			}
		}

		delete(existing, song.ID)
	}

	for id := range existing {
		if _, err = tx.ExecContext(ctx, "DELETE FROM songs WHERE collection_id=? AND id=?", c.ID, id); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *Store) Update(ctx context.Context, id string, fn func(*domain.Collection) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.get(ctx, id)
	if err != nil {
		return err
	}

	if err = fn(&c); err != nil {
		return err
	}

	return s.save(ctx, c)
}

func (s *Store) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.db.ExecContext(ctx, "DELETE FROM collections WHERE id=?", id)
	if err != nil {
		return err
	}

	n, _ := r.RowsAffected()

	if n == 0 {
		return errors.New("collection not found")
	}

	return nil
}
