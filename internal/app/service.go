package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kristakoch/break-up-playlist/internal/domain"
)

type Service struct {
	Store                 Store
	Model                 Classifier
	Lyrics                Lyrics
	FallbackLyrics        Lyrics
	Music                 Music
	mu                    sync.Mutex
	cancel                context.CancelFunc
	active, kind, waiting string
	waitingBypass         bool
	waitingRetry          bool
	stopped               map[string]bool
	root                  context.Context
	wg                    sync.WaitGroup
}

func New(ctx context.Context, s Store, m Classifier, l Lyrics, t Music) *Service {
	return &Service{Store: s, Model: m, Lyrics: l, Music: t, root: ctx, stopped: map[string]bool{}}
}

func ID() string {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		panic(err)
	}

	return hex.EncodeToString(b)
}

func (s *Service) Recover(ctx context.Context) error {
	cs, err := s.Store.List(ctx)
	if err != nil {
		return err
	}

	for _, c := range cs {
		if err = s.Store.Update(ctx, c.ID, func(c *domain.Collection) error {
			if c.Job.State == "running" || c.Job.State == "waiting" {
				c.Job.State = "paused"
				c.Job.Message = "Interrupted by restart; completed work is saved."
			}

			for i := range c.Songs {
				if suggestion := c.Songs[i].LyricSuggestion; suggestion != nil && strings.TrimSpace(c.Songs[i].Lyrics) == "" && c.Songs[i].LyricStatus != "manual" {
					c.Songs[i].Lyrics = suggestion.Lyrics
					c.Songs[i].LyricSource = suggestion.Source
					c.Songs[i].LyricStatus = "found"
					c.Songs[i].LyricSuggestion = nil
					c.Songs[i].Prediction = nil
					c.Revision++
				}

				p := c.Songs[i].Prediction

				if p != nil && (p.Version != domain.InstructionVersion || domain.ValidatePrediction(*p, c.Categories) != nil) {
					p.Review = true
				}
			}

			return nil
		}); err != nil {
			return err
		}
	}

	return nil
}

func (s *Service) Wait() { s.wg.Wait() }

func (s *Service) EditSong(ctx context.Context, cid, sid, lyrics, desc, manual, ref string) error {
	return s.Store.Update(ctx, cid, func(c *domain.Collection) error {
		song := c.Song(sid)

		if song == nil {
			return errors.New("song not found")
		}

		if manual != "" && !c.HasCategory(manual) || ref != "" && !c.HasCategory(ref) {
			return errors.New("unknown category")
		}

		if song.Lyrics != lyrics || song.Description != desc {
			song.Prediction = nil
			song.Error = ""
		}

		if song.Lyrics != lyrics {
			song.LyricStatus = "manual"
			song.LyricSource = "Manual entry"
		}

		if strings.TrimSpace(lyrics) != "" {
			song.LyricSuggestion = nil
		}

		song.Lyrics = lyrics
		song.Description = desc
		song.Manual = manual
		song.Reference = ref
		c.Revision++

		return nil
	})
}

func (s *Service) Category(ctx context.Context, cid, id, name, desc string, remove bool) error {
	return s.Store.Update(ctx, cid, func(c *domain.Collection) error {
		cats := append([]domain.Category(nil), c.Categories...)

		if id == "" {
			cats = append(cats, domain.Category{ID: ID(), Name: strings.TrimSpace(name), Description: strings.TrimSpace(desc)})
		} else {
			found := false

			for i, cat := range cats {
				if cat.ID != id {
					continue
				}

				found = true

				if remove {
					if domain.Referenced(*c, id) {
						return errors.New("category is referenced: reassign songs, clear reference labels and reset predictions first")
					}

					cats = append(cats[:i], cats[i+1:]...)
				} else {
					cats[i].Name = strings.TrimSpace(name)
					cats[i].Description = strings.TrimSpace(desc)
				}

				break
			}

			if !found {
				return errors.New("category not found")
			}
		}

		if err := domain.ValidateCategories(cats); err != nil {
			return err
		}

		if slices.Equal(c.Categories, cats) {
			return nil
		}

		c.Categories = cats

		for i := range c.Songs {
			if c.Songs[i].Prediction != nil {
				c.Songs[i].Prediction.Review = true
			}
		}

		c.Revision++

		return nil
	})
}

func (s *Service) Reset(ctx context.Context, cid string, manual bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.active == cid {
		return errors.New("stop the active job before resetting")
	}

	return s.Store.Update(ctx, cid, func(c *domain.Collection) error {
		for i := range c.Songs {
			c.Songs[i].Prediction = nil
			c.Songs[i].Error = ""

			if manual {
				c.Songs[i].Manual = ""
			}
		}

		c.Revision++

		return nil
	})
}

func (s *Service) Remove(ctx context.Context, cid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.active == cid {
		return errors.New("stop the active job before removing this collection")
	}

	return s.Store.Delete(ctx, cid)
}

// Import stages provider reads and atomically merges against the latest saved human edits.
func (s *Service) Import(ctx context.Context, id string) (string, error) {
	s.mu.Lock()

	if s.active != "" {
		s.mu.Unlock()

		return "", errors.New("stop the active job before importing")
	}

	s.active, s.kind = "import", "import"
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.active, s.kind = "", ""; s.mu.Unlock() }()
	name, tracks, err := s.Music.Import(ctx, id)
	if err != nil {
		return "", err
	}

	collections, err := s.Store.List(ctx)
	if err != nil {
		return "", err
	}

	merge := func(c *domain.Collection) error {
		old := map[string]domain.Song{}

		for _, song := range c.Songs {
			old[song.ID] = song
		}

		seen := map[string]bool{}
		songs := []domain.Song{}

		for pos, m := range tracks {
			if m.ID == "" {
				return errors.New("provider returned a track without an ID; import is incomplete")
			}

			if seen[m.ID] {
				continue
			}

			seen[m.ID] = true
			song := old[m.ID]
			before, _ := json.Marshal(song.Metadata)
			after, _ := json.Marshal(m.Metadata)

			if string(before) != string(after) {
				song.Prediction = nil
				song.Error = ""
				song.LyricsSearchVersion = 0
				song.LyricsOVHStatus = ""
				song.LyricSuggestion = nil
			}

			song.Metadata = m.Metadata
			song.Position = pos + 1
			if strings.TrimSpace(m.Lyrics) != "" && song.LyricStatus != "manual" && song.LyricSource != "Manual entry" {
				if song.Lyrics != m.Lyrics {
					song.Prediction = nil
					song.Error = ""
				}
				song.Lyrics = m.Lyrics
				song.LyricStatus = "found"
				song.LyricSource = "TIDAL"
			}

			if song.LyricStatus == "" {
				song.LyricStatus = "unchecked"
			}

			songs = append(songs, song)
		}

		c.Name = name
		c.Songs = songs
		c.Revision++

		return nil
	}

	for _, c := range collections {
		if c.SourceID == id {
			return c.ID, s.Store.Update(ctx, c.ID, merge)
		}
	}

	c := domain.Collection{ID: ID(), SourceID: id}
	if err = merge(&c); err != nil {
		return "", err
	}

	return c.ID, s.Store.Save(ctx, c)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Assign changes only the manual choice, so a row form cannot overwrite evidence.
func (s *Service) Assign(ctx context.Context, cid, sid, category string) error {
	return s.Store.Update(ctx, cid, func(c *domain.Collection) error {
		song := c.Song(sid)

		if song == nil {
			return errors.New("song not found")
		}

		if category != "" && !c.HasCategory(category) {
			return errors.New("unknown category")
		}

		song.Manual = category
		c.Revision++

		return nil
	})
}
