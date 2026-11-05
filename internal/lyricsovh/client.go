// Package lyricsovh implements the public Lyrics.ovh lyrics lookup.
package lyricsovh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kristakoch/break-up-playlist/internal/app"
	"github.com/kristakoch/break-up-playlist/internal/domain"
)

type Client struct {
	Base string
	HTTP *http.Client
}

func New() *Client {
	return &Client{Base: "https://api.lyrics.ovh", HTTP: &http.Client{Timeout: 30 * time.Second}}
}

type requestError struct {
	status int
	delay  time.Duration
}

func (e requestError) Error() string {
	return fmt.Sprintf("Lyrics.ovh unavailable (HTTP %d)", e.status)
}
func (e requestError) RetryDelay() time.Duration { return e.delay }
func (e requestError) Temporary() bool {
	return e.status == http.StatusTooManyRequests || e.status >= 500
}

func (c *Client) Lookup(ctx context.Context, m domain.Metadata) (app.LyricResult, error) {
	if strings.TrimSpace(m.Artist) == "" || strings.TrimSpace(m.Title) == "" {
		return app.LyricResult{Status: "missing metadata"}, nil
	}

	address := c.Base + "/v1/" + url.PathEscape(m.Artist) + "/" + url.PathEscape(m.Title)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return app.LyricResult{}, err
	}
	req.Header.Set("User-Agent", "BreakUpPlaylist/0.1 (https://github.com/kristakoch/break-up-playlist)")
	response, err := c.HTTP.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return app.LyricResult{}, fmt.Errorf("Lyrics.ovh request failed: %w", urlErr.Err)
		}
		return app.LyricResult{}, errors.New("Lyrics.ovh request failed")
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusNotFound {
		return app.LyricResult{Status: "missing"}, nil
	}
	if response.StatusCode != http.StatusOK {
		seconds, _ := strconv.Atoi(response.Header.Get("Retry-After"))
		return app.LyricResult{}, requestError{response.StatusCode, time.Duration(seconds) * time.Second}
	}

	var result struct{ Lyrics string }
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&result); err != nil {
		return app.LyricResult{}, err
	}
	if strings.TrimSpace(result.Lyrics) == "" {
		return app.LyricResult{Status: "missing"}, nil
	}

	return app.LyricResult{Lyrics: result.Lyrics, Source: "Lyrics.ovh", Status: "found"}, nil
}
