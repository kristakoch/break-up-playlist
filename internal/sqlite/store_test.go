package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/kristakoch/break-up-playlist/internal/domain"
)

func TestRestartAndFailedMutation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	c := domain.Collection{ID: "c", Songs: []domain.Song{{Metadata: domain.Metadata{ID: "song"}, Lyrics: "Human lyrics", Manual: "a", Reference: "b", Prediction: &domain.Prediction{Category: "b"}}}, Exports: []domain.Export{{ID: "export", Confirmed: true, Playlists: []domain.ExportPlaylist{{Destination: "new", Batches: []domain.Batch{{Key: "stable", Done: true}}}}}}}
	if err = s.Save(ctx, c); err != nil {
		t.Fatal(err)
	}

	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}

	defer s.Close()
	got, err := s.Get(ctx, "c")
	if err != nil || got.Songs[0].Lyrics != "Human lyrics" || got.Songs[0].Prediction.Category != "b" || !got.Exports[0].Playlists[0].Batches[0].Done {
		t.Fatal("restart lost state", err)
	}
}

func TestVersionOneMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(`CREATE TABLE schema_versions(version INTEGER PRIMARY KEY); INSERT INTO schema_versions VALUES(1); CREATE TABLE collections(id TEXT PRIMARY KEY,document BLOB NOT NULL);`)
	if err != nil {
		t.Fatal(err)
	}

	c := domain.Collection{ID: "c", Songs: []domain.Song{{Metadata: domain.Metadata{ID: "song"}, Lyrics: "Keep these lyrics", Manual: "a", Position: 1}}}
	b, _ := json.Marshal(c)
	if _, err = db.Exec("INSERT INTO collections VALUES(?,?)", c.ID, b); err != nil {
		t.Fatal(err)
	}

	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	defer s.Close()
	got, err := s.Get(context.Background(), "c")
	if err != nil || len(got.Songs) != 1 || got.Songs[0].Lyrics != "Keep these lyrics" || got.Songs[0].Manual != "a" {
		t.Fatal("migration lost state", err)
	}
}
func TestProgressDoesNotRewriteSongsAndFailedSaveRollsBack(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}

	defer s.Close()
	c := domain.Collection{ID: "c", Name: "Original", Songs: []domain.Song{{Metadata: domain.Metadata{ID: "song"}, Lyrics: "Lyrics", Position: 1}}}
	if err = s.Save(ctx, c); err != nil {
		t.Fatal(err)
	}

	_, err = s.db.Exec(`CREATE TABLE writes(n INTEGER); INSERT INTO writes VALUES(0); CREATE TRIGGER song_updates AFTER UPDATE ON songs BEGIN UPDATE writes SET n=n+1; END;`)
	if err != nil {
		t.Fatal(err)
	}

	if err = s.Update(ctx, "c", func(c *domain.Collection) error { c.Job.Processed++; return nil }); err != nil {
		t.Fatal(err)
	}

	var count int
	s.db.QueryRow("SELECT n FROM writes").Scan(&count)

	if count != 0 {
		t.Fatal("progress rewrote saved evidence")
	}

	c.Name = "Should roll back"
	c.Songs = append(c.Songs, c.Songs[0])
	if err = s.Save(ctx, c); err == nil {
		t.Fatal("duplicate song accepted")
	}

	got, err := s.Get(ctx, "c")
	if err != nil || got.Name != "Original" || len(got.Songs) != 1 {
		t.Fatal("partial transaction committed", err)
	}

	if err = s.Delete(ctx, "c"); err != nil {
		t.Fatal(err)
	}

	s.db.QueryRow("SELECT count(*) FROM songs").Scan(&count)

	if count != 0 {
		t.Fatal("local song rows were not removed")
	}
}
