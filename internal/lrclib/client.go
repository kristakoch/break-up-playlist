// Package lrclib implements anonymous, conservatively matched lyric lookup.
package lrclib

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/kristakoch/break-up-playlist/internal/app"
	"github.com/kristakoch/break-up-playlist/internal/domain"
)

type Client struct {
	Base string
	HTTP *http.Client
}

func New() *Client {
	return &Client{Base: "https://lrclib.net", HTTP: &http.Client{Timeout: 30 * time.Second}}
}

type requestError struct {
	status int
	delay  time.Duration
}

func (e requestError) Error() string {
	return fmt.Sprintf("lyrics provider unavailable (HTTP %d); retry later", e.status)
}

func (e requestError) Temporary() bool { return e.status == 429 || e.status >= 500 }

func (e requestError) RetryDelay() time.Duration { return e.delay }

func norm(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}

		return -1
	}, s)
}

func (c *Client) Lookup(ctx context.Context, m domain.Metadata) (app.LyricResult, error) {
	out := app.LyricResult{Status: "missing metadata"}

	if m.Title == "" || m.Artist == "" {
		return out, nil
	}

	q := url.Values{"track_name": {m.Title}, "artist_name": {m.Artist}}

	req, _ := http.NewRequestWithContext(ctx, "GET", c.Base+"/api/search?"+q.Encode(), nil)
	req.Header.Set("User-Agent", "BreakUpPlaylist/0.1 (https://github.com/kristakoch/break-up-playlist)")
	r, err := c.HTTP.Do(req)
	if err != nil {
		return out, err
	}

	defer r.Body.Close()

	if r.StatusCode != 200 {
		delay := time.Duration(0)

		if v, e := strconv.Atoi(r.Header.Get("Retry-After")); e == nil {
			delay = time.Duration(v) * time.Second
		} else if t, e := http.ParseTime(r.Header.Get("Retry-After")); e == nil {
			delay = time.Until(t)
		}

		return out, requestError{r.StatusCode, delay}
	}

	var entries []struct {
		ID                               int
		TrackName, ArtistName, AlbumName string
		Duration                         float64
		Instrumental                     bool
		PlainLyrics, SyncedLyrics        string
	}

	if err = json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&entries); err != nil {
		return out, err
	}

	out.Status = "missing"

	if len(entries) > 0 {
		out.Status = "mismatch"
	}

	var candidates []app.LyricResult
	var exact []app.LyricResult
	for _, e := range entries {
		if norm(e.TrackName) != norm(m.Title) || norm(e.ArtistName) != norm(m.Artist) {
			continue
		}

		lyrics := e.PlainLyrics
		if strings.TrimSpace(lyrics) == "" {
			lyrics = e.SyncedLyrics
		}
		if e.Instrumental || strings.TrimSpace(lyrics) == "" {
			if e.Instrumental && (m.Duration <= 0 || math.Abs(e.Duration-m.Duration) <= 2) {
				out.Status = "instrumental"
			}
			continue
		}

		source := fmt.Sprintf("https://lrclib.net/api/get/%d", e.ID)
		if (m.Album == "" || norm(e.AlbumName) == norm(m.Album)) && (m.Duration <= 0 || math.Abs(e.Duration-m.Duration) <= 2) {
			exact = append(exact, app.LyricResult{Lyrics: lyrics, Source: source, Status: "found"})
			continue
		}

		candidates = append(candidates, app.LyricResult{Lyrics: lyrics, Source: source, Status: "found"})
	}

	if len(exact) > 0 {
		return exact[0], nil
	}

	if len(candidates) > 0 {
		return candidates[0], nil
	}
	return out, nil
}
