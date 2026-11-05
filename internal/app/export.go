package app

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/kristakoch/break-up-playlist/internal/domain"
)

func (s *Service) Preview(ctx context.Context, cid string) error {
	return s.Store.Update(ctx, cid, func(c *domain.Collection) error {
		if c.SourceID == "" {
			return errors.New("only TIDAL-imported collections can be exported")
		}

		for _, e := range c.Exports {
			if e.Confirmed && e.State != "completed" {
				return errors.New("resume the existing confirmed export first")
			}
		}

		e := domain.Export{ID: ID(), Revision: c.Revision, State: "preview"}

		for _, song := range c.Songs {
			if !c.HasCategory(song.Assigned()) {
				return fmt.Errorf("track %s needs a valid assignment", song.DisplayTitle())
			}
		}

		for _, cat := range c.Categories {
			p := domain.ExportPlaylist{Category: cat.ID, Name: cat.Name, CreateKey: ID()}

			for _, song := range c.Songs {
				if song.Assigned() == cat.ID {
					p.Tracks = append(p.Tracks, song.Metadata)
				}
			}

			if len(p.Tracks) == 0 {
				continue
			}

			if utf8.RuneCountInString(p.Name) > 250 {
				return errors.New("playlist name exceeds TIDAL's 250-character limit; shorten the category name")
			}

			for i := 0; i < len(p.Tracks); i += 50 {
				b := domain.Batch{Key: ID()}
				end := min(i+50, len(p.Tracks))

				for _, track := range p.Tracks[i:end] {
					b.IDs = append(b.IDs, track.ID)
				}

				p.Batches = append(p.Batches, b)
			}

			e.Playlists = append(e.Playlists, p)
		}

		if len(e.Playlists) == 0 {
			return errors.New("there are no tracks to export")
		}

		kept := []domain.Export{}

		for _, old := range c.Exports {
			if old.Confirmed {
				kept = append(kept, old)
			}
		}

		c.Exports = append(kept, e)

		return nil
	})
}

func (s *Service) Confirm(ctx context.Context, cid, eid string) error {
	return s.Store.Update(ctx, cid, func(c *domain.Collection) error {
		for i := range c.Exports {
			e := &c.Exports[i]

			if e.ID != eid {
				continue
			}

			if e.Confirmed {
				return errors.New("export already confirmed; use resume")
			}

			if e.Revision != c.Revision {
				return errors.New("preview is stale; generate a fresh preview")
			}

			e.Confirmed = true
			e.State = "confirmed"

			return nil
		}

		return errors.New("preview not found")
	})
}

func (s *Service) export(ctx context.Context, cid string) error {
	c, err := s.Store.Get(ctx, cid)
	if err != nil {
		return err
	}

	idx := -1

	for i, e := range c.Exports {
		if e.Confirmed && e.State != "completed" {
			idx = i
			break
		}
	}

	if idx < 0 {
		return errors.New("no incomplete confirmed export")
	}

	e := c.Exports[idx]
	save := func() error {
		return s.Store.Update(ctx, cid, func(c *domain.Collection) error {
			for i := range c.Exports {
				if c.Exports[i].ID == e.ID {
					c.Exports[i] = e

					return nil
				}
			}

			return errors.New("confirmed export disappeared")
		})
	}

	fail := func(err error) error {
		e.State = "paused"
		e.Error = err.Error()
		_ = s.Store.Update(context.Background(), cid, func(c *domain.Collection) error {
			for i := range c.Exports {
				if c.Exports[i].ID == e.ID {
					c.Exports[i] = e
					return nil
				}
			}
			return errors.New("confirmed export disappeared")
		})

		return err
	}

	if err = s.Store.Update(ctx, cid, func(c *domain.Collection) error {
		c.Job = domain.Job{Kind: "export", State: "running", Total: len(e.Playlists)}

		return nil
	}); err != nil {
		return err
	}

	for i := range e.Playlists {
		p := &e.Playlists[i]

		if p.Destination == "" {
			err = s.attempt(ctx, func() error {
				var err error
				p.Destination, err = s.Music.Create(ctx, p.Name, p.CreateKey)

				return err
			})
			if err != nil {
				return fail(err)
			}

			if p.Destination == "" || p.Destination == c.SourceID {
				return fail(errors.New("provider returned an unsafe destination; no tracks were added"))
			}

			for j, other := range e.Playlists {
				if j != i && other.Destination == p.Destination {
					return fail(errors.New("provider reused a destination; export paused"))
				}
			}

			if err = save(); err != nil {
				return err
			}
		}

		if p.Destination == c.SourceID {
			return fail(errors.New("source playlist cannot be an export destination"))
		}

		for j := range p.Batches {
			b := &p.Batches[j]

			if b.Done {
				continue
			}

			for len(b.IDs) > 0 {
				err = s.attempt(ctx, func() error { return s.Music.Add(ctx, p.Destination, b.IDs, b.Key) })
				if err == nil {
					break
				}

				var unavailable *domain.UnavailableTracksError
				if !errors.As(err, &unavailable) || len(unavailable.IDs) == 0 {
					return fail(err)
				}

				missing := make(map[string]bool, len(unavailable.IDs))
				for _, id := range unavailable.IDs {
					missing[id] = true
				}
				remaining := make([]string, 0, len(b.IDs))
				for _, id := range b.IDs {
					if !missing[id] {
						remaining = append(remaining, id)
						continue
					}
					for _, track := range p.Tracks {
						if track.ID == id {
							e.Omitted = append(e.Omitted, track)
							break
						}
					}
					delete(missing, id)
				}
				if len(missing) != 0 || len(remaining) == len(b.IDs) {
					return fail(errors.New("provider reported an unexpected unavailable track"))
				}

				b.IDs = remaining
				b.Key = ID()
				e.Unavailable = nil
				if err = save(); err != nil {
					return err
				}
			}

			b.Done = true
			if err = save(); err != nil {
				return err
			}
		}

		if err = s.Store.Update(ctx, cid, func(c *domain.Collection) error { c.Job.Processed = i + 1; return nil }); err != nil {
			return err
		}
	}

	e.State = "completed"
	e.Error = ""
	e.Unavailable = nil

	return save()
}

// RemoveExportHistory removes a completed local export record, not TIDAL playlists.
func (s *Service) RemoveExportHistory(ctx context.Context, cid, eid string) error {
	return s.Store.Update(ctx, cid, func(c *domain.Collection) error {
		for i, export := range c.Exports {
			if export.ID != eid {
				continue
			}
			if !export.Confirmed || export.State != "completed" {
				return errors.New("only completed exports can be removed from history")
			}

			c.Exports = append(c.Exports[:i], c.Exports[i+1:]...)
			return nil
		}
		return errors.New("export not found")
	})
}
