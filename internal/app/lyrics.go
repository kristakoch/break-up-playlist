package app

import (
	"context"
	"log/slog"
	"reflect"
	"strings"
	"time"

	"github.com/kristakoch/break-up-playlist/internal/domain"
)

func (s *Service) lookupLyrics(ctx context.Context, cid string, before domain.Song, fallback bool) error {
	provider := s.Lyrics
	providerName := "LRCLIB"
	if fallback {
		provider = s.FallbackLyrics
		providerName = "Lyrics.ovh"
	}

	logger := slog.With("provider", providerName, "collection", cid, "track_id", before.ID,
		"title", before.Title, "artist", before.Artist)
	attempt := 0
	var result LyricResult
	err := s.attempt(ctx, func() error {
		attempt++
		started := time.Now()
		logger.Info("lyrics lookup started", "attempt", attempt)

		var err error
		result, err = provider.Lookup(ctx, before.Metadata)
		if err != nil {
			logger.Warn("lyrics lookup attempt failed", "attempt", attempt, "elapsed", time.Since(started), "error", err)
		} else {
			logger.Info("lyrics lookup finished", "attempt", attempt, "elapsed", time.Since(started), "status", result.Status)
		}

		return err
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}

	saved := false
	saveErr := s.Store.Update(ctx, cid, func(c *domain.Collection) error {
		song := c.Song(before.ID)
		if song == nil || song.Lyrics != before.Lyrics || song.LyricStatus != before.LyricStatus || !reflect.DeepEqual(song.Metadata, before.Metadata) {
			return nil
		}

		saved = true
		status := result.Status
		if err != nil {
			status = "failed"
			c.Job.Failed++
		}
		if fallback {
			song.LyricsOVHStatus = status
		} else {
			song.LyricsSearchVersion = 1
			song.LyricStatus = status
		}

		if err == nil && strings.TrimSpace(result.Lyrics) != "" {
			song.Lyrics = result.Lyrics
			song.LyricSource = result.Source
			song.LyricStatus = "found"
			song.LyricSuggestion = nil
			song.Prediction = nil
			c.Revision++
		}

		return nil
	})
	if saveErr != nil {
		logger.Error("lyrics lookup result could not be saved", "error", saveErr)
		return saveErr
	}
	if !saved {
		logger.Info("lyrics lookup result discarded because the song changed")
	}

	return err
}

// runLyrics processes LRCLIB sequentially while one worker handles fallback requests.
func (s *Service) runLyrics(ctx context.Context, cid string, songs []domain.Song, retry bool) error {
	queue := make(chan string, len(songs))
	finished := make(chan error, 1)

	go func() {
		available := true
		for id := range queue {
			if !available {
				continue
			}

			if err := ctx.Err(); err != nil {
				finished <- err
				return
			}

			collection, err := s.Store.Get(ctx, cid)
			if err != nil {
				finished <- err
				return
			}

			song := collection.Song(id)
			if song != nil && fallbackLyricsEligible(*song, retry) {
				if err := s.lookupLyrics(ctx, cid, *song, true); err != nil {
					if ctx.Err() != nil {
						finished <- ctx.Err()
						return
					}

					slog.Warn("Lyrics.ovh lookup paused; continuing LRCLIB", "error", err)
					available = false
					s.mu.Lock()
					s.stopped[cid] = true
					s.mu.Unlock()

					if err := s.Store.Update(ctx, cid, func(c *domain.Collection) error {
						c.Job.Message = "Lyrics.ovh is unavailable; LRCLIB lookup continued. Try fetching lyrics again later."
						return nil
					}); err != nil {
						finished <- err
						return
					}
				}
			}

			if err := s.lyricsProcessed(ctx, cid); err != nil {
				finished <- err
				return
			}

			if available {
				if err := sleep(ctx, time.Second); err != nil {
					finished <- err
					return
				}
			}
		}
		finished <- nil
	}()

	var primaryErr error
	for _, original := range songs {
		if primaryErr = ctx.Err(); primaryErr != nil {
			break
		}

		collection, err := s.Store.Get(ctx, cid)
		if err != nil {
			primaryErr = err
			break
		}

		song := collection.Song(original.ID)
		if song == nil || !jobIncludesSong("lyrics", *song, retry) {
			continue
		}

		if lyricsEligible(*song, retry) {
			primaryErr = s.lookupLyrics(ctx, cid, *song, false)
			if ctx.Err() != nil {
				primaryErr = ctx.Err()
				break
			}

			collection, err = s.Store.Get(ctx, cid)
			if err != nil {
				primaryErr = err
				break
			}
			song = collection.Song(original.ID)
		}

		if song != nil && s.FallbackLyrics != nil && fallbackLyricsEligible(*song, retry) {
			queue <- song.ID
		} else if err := s.lyricsProcessed(ctx, cid); err != nil {
			primaryErr = err
			break
		}

		if primaryErr != nil {
			break
		}

		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			primaryErr = err
			break
		}
	}

	close(queue)
	fallbackErr := <-finished
	if primaryErr != nil {
		return primaryErr
	}
	return fallbackErr
}

func (s *Service) lyricsProcessed(ctx context.Context, cid string) error {
	return s.Store.Update(ctx, cid, func(c *domain.Collection) error {
		c.Job.Processed++
		return nil
	})
}
