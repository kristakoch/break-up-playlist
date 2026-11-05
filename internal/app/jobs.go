package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/kristakoch/break-up-playlist/internal/domain"
)

// Start serializes evidence jobs globally; only classification may wait behind lyrics.
// For evidence jobs, retry includes failed lookups; completed lookups are kept.
// For classification, retry limits the run to songs without a saved prediction.
func (s *Service) Start(cid, kind string, retry, bypass bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if kind != "lyrics" && kind != "classification" && kind != "export" {
		return errors.New("unknown job")
	}

	if s.active != "" {
		if kind == "classification" && s.kind == "lyrics" && s.active == cid {
			s.waiting = cid
			s.waitingBypass = bypass
			s.waitingRetry = retry

			return nil
		}

		return errors.New("another job is running")
	}

	if _, err := s.Store.Get(s.root, cid); err != nil {
		return err
	}

	if kind == "lyrics" {
		delete(s.stopped, cid)
	}

	return s.launch(cid, kind, retry, bypass)
}

// jobIncludesSong keeps progress totals aligned with each job's eligibility rules.
func jobIncludesSong(kind string, song domain.Song, retry bool) bool {
	switch kind {
	case "classification":
		return song.Eligible() && (!retry || song.Prediction == nil)
	case "lyrics":
		return lyricsEligible(song, retry) || fallbackLyricsEligible(song, retry)
	default:
		return false
	}
}

func lyricsEligible(song domain.Song, retry bool) bool {
	return strings.TrimSpace(song.Lyrics) == "" && song.LyricStatus != "manual" &&
		(song.LyricsSearchVersion < 1 || (retry && song.LyricStatus == "failed"))
}

func fallbackLyricsEligible(song domain.Song, retry bool) bool {
	return strings.TrimSpace(song.Lyrics) == "" && song.LyricStatus != "manual" &&
		(song.LyricsOVHStatus == "" || (retry && song.LyricsOVHStatus == "failed"))
}

func (s *Service) launch(cid, kind string, retry, bypass bool) error {
	if err := s.Store.Update(s.root, cid, func(c *domain.Collection) error {
		total := 0

		for _, song := range c.Songs {
			if jobIncludesSong(kind, song, retry) {
				total++
			}
		}

		c.Job = domain.Job{Kind: kind, State: "running", Total: total}

		return nil
	}); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(s.root)
	s.cancel = cancel
	s.active = cid
	s.kind = kind
	s.wg.Add(1)

	go func() {
		defer s.wg.Done()
		defer cancel()
		err := s.run(ctx, cid, kind, retry, bypass)
		state, message := "completed", "Completed"
		if errors.Is(err, context.Canceled) {
			state, message = "stopped", "Stopped; completed work is saved."
		} else if err != nil {
			slog.Error("background job paused", "kind", kind, "collection", cid, "error", err)
			state, message = "paused", err.Error()
		}

		if saveErr := s.Store.Update(context.Background(), cid, func(c *domain.Collection) error {
			if state == "completed" && c.Job.Message != "" {
				message = c.Job.Message
			}
			c.Job.State = state
			c.Job.Message = message
			return nil
		}); saveErr != nil {
			fmt.Printf("job checkpoint failed: %v\n", saveErr)
		}

		s.mu.Lock()
		defer s.mu.Unlock()

		if (kind == "lyrics") && err != nil {
			s.stopped[cid] = true
		}

		s.active = ""
		s.kind = ""
		s.cancel = nil
		if s.waiting == cid && err == nil {
			s.waiting = ""

			if nextErr := s.launch(cid, "classification", s.waitingRetry, s.waitingBypass); nextErr != nil {
				fmt.Printf("queued job could not start: %v\n", nextErr)
			}
		} else {
			s.waiting = ""
		}
	}()

	return nil
}

func (s *Service) Stop(cid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped[cid] = true
	s.waiting = ""

	if s.active == cid && s.cancel != nil {
		s.cancel()
	}
}

func (s *Service) Leave(cid string) { s.mu.Lock(); defer s.mu.Unlock(); delete(s.stopped, cid) }

func (s *Service) AutoEvidence(cid string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.active != "" || s.stopped[cid] || s.Lyrics == nil {
		return
	}

	c, err := s.Store.Get(s.root, cid)
	if err != nil || c.SourceID == "" {
		return
	}

	for _, song := range c.Songs {
		if lyricsEligible(song, false) || (s.FallbackLyrics != nil && fallbackLyricsEligible(song, false)) {
			if err := s.launch(cid, "lyrics", false, false); err != nil {
				fmt.Printf("lyrics job could not start: %v\n", err)
			}

			return
		}
	}

}

func (s *Service) Status(ctx context.Context, cid string) (domain.Job, error) {
	c, err := s.Store.Get(ctx, cid)
	if err != nil {
		return domain.Job{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	j := c.Job

	if s.waiting == cid {
		j.Message = "Fetching " + s.kind + "; classification is waiting."
	}

	return j, nil
}

func (s *Service) run(ctx context.Context, cid, kind string, retry, bypass bool) error {
	c, err := s.Store.Get(ctx, cid)
	if err != nil {
		return err
	}

	if kind == "export" {
		return s.export(ctx, cid)
	}

	model := ""

	if kind == "classification" {
		if len(c.Categories) == 0 {
			return errors.New("add at least one category")
		}

		model, err = s.Model.Identity(ctx)
		if err != nil {
			return err
		}
	}

	total := 0

	for _, song := range c.Songs {
		if jobIncludesSong(kind, song, retry) {
			total++
		}
	}

	if err = s.Store.Update(ctx, cid, func(c *domain.Collection) error {
		c.Job = domain.Job{Kind: kind, State: "running", Total: total}

		return nil
	}); err != nil {
		return err
	}

	if kind == "lyrics" {
		return s.runLyrics(ctx, cid, c.Songs, retry)
	}

	for _, original := range c.Songs {
		if err = ctx.Err(); err != nil {
			return err
		}

		fresh, e := s.Store.Get(ctx, cid)

		if e != nil {
			return e
		}

		song := fresh.Song(original.ID)

		if song == nil {
			continue
		}

		if !song.Eligible() || (retry && song.Prediction != nil) {
			continue
		}

		fp := domain.Fingerprint(*song, fresh.Categories, model)

		if !bypass && song.Prediction != nil && song.Prediction.Fingerprint == fp {
			err = s.Store.Update(ctx, cid, func(c *domain.Collection) error {
				v := c.Song(original.ID)

				if v != nil && v.Prediction != nil && domain.Fingerprint(*v, c.Categories, model) == fp {
					if v.Prediction.Review {
						v.Prediction.Review = false
						c.Revision++
					}

					v.Error = ""
					c.Job.Cached++
				}

				c.Job.Processed++

				return nil
			})
			if err != nil {
				return err
			}

			continue
		}

		request := domain.ChoiceRequest{Evidence: song.Evidence(), Categories: fresh.Categories}
		var p domain.Prediction
		err = s.attempt(ctx, func() error { var e error; p, e = s.Model.Classify(ctx, request); return e })
		if err != nil && !errors.Is(err, ErrInvalidOutput) {
			if ctx.Err() == nil {
				requestErr := err

				if saveErr := s.Store.Update(context.Background(), cid, func(c *domain.Collection) error {
					if v := c.Song(original.ID); v != nil {
						v.Error = requestErr.Error()
					}

					c.Job.Failed++

					return nil
				}); saveErr != nil {
					return saveErr
				}
			}

			return err
		}

		validation := err

		if validation == nil {
			validation = domain.ValidatePrediction(p, request.Categories)
		}

		p.Model = model
		p.Version = domain.InstructionVersion
		p.Fingerprint = fp
		err = s.Store.Update(ctx, cid, func(c *domain.Collection) error {
			v := c.Song(original.ID)

			if v == nil {
				return nil
			}

			c.Job.Processed++

			if domain.Fingerprint(*v, c.Categories, model) != fp {
				c.Job.Message = "Inputs changed during classification; rerun to classify updated songs."

				return nil
			}

			if validation != nil {
				slog.Error("classification result rejected", "collection", cid, "track", original.ID, "error", validation)
				v.Error = validation.Error()
				c.Job.Failed++

				return nil
			}

			v.Prediction = &p
			v.Error = ""
			c.Revision++

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

type temporary interface{ Temporary() bool }

func (s *Service) attempt(ctx context.Context, fn func() error) error {
	var err error

	for i := 0; i < 4; i++ {
		if err = ctx.Err(); err != nil {
			return err
		}

		err = fn()
		if err == nil {
			return nil
		}

		var t temporary
		slog.Warn("provider request failed", "attempt", i+1, "error", err)

		if !errors.As(err, &t) || !t.Temporary() {
			return err
		}

		if i < 3 {
			delay := retryDelay(err, i, rand.Float64())
			slog.Info("waiting before provider retry", "delay", delay, "next_attempt", i+2)

			if err = sleep(ctx, delay); err != nil {
				return err
			}
		}
	}

	return fmt.Errorf("provider unavailable after retries; resume later: %w", err)
}

// retryDelay respects the provider's minimum wait and adds up to 30% jitter.
func retryDelay(err error, attempt int, jitter float64) time.Duration {
	delay := time.Second * time.Duration(1<<attempt)
	var r interface{ RetryDelay() time.Duration }
	if errors.As(err, &r) && r.RetryDelay() > 0 {
		providerDelay := r.RetryDelay()
		delay = max(delay, providerDelay)
		delay += time.Duration(float64(providerDelay) * 0.3 * jitter)
	}
	return delay
}
