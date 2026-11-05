package lrclib

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kristakoch/break-up-playlist/internal/domain"
)

func TestMatchingAndInstrumentals(t *testing.T) {
	tests := []struct{ name, body, status, lyrics string }{{"exact", `[{"id":1,"trackName":"Song","artistName":"Artist","albumName":"Album","duration":180,"plainLyrics":"Actual lyrics"}]`, "found", "Actual lyrics"}, {"wrong recording", `[{"id":1,"trackName":"Song live","artistName":"Artist","albumName":"Album","duration":180,"plainLyrics":"Wrong lyrics"}]`, "mismatch", ""}, {"wrong duration", `[{"id":1,"trackName":"Song","artistName":"Artist","albumName":"Album","duration":190,"plainLyrics":"Wrong lyrics"}]`, "found", "Wrong lyrics"}, {"instrumental", `[{"id":1,"trackName":"Song","artistName":"Artist","albumName":"Album","duration":180,"instrumental":true}]`, "instrumental", ""}, {"missing", `[]`, "missing", ""}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("User-Agent") == "" {
					t.Error("missing identification")
				}

				fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()
			c := New()
			c.Base = srv.URL
			got, err := c.Lookup(context.Background(), domain.Metadata{Title: "Song", Artist: "Artist", Album: "Album", Duration: 180})
			if err != nil || got.Status != tt.status || got.Lyrics != tt.lyrics {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
}

func TestRateLimitDelay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Retry-After", "12"); w.WriteHeader(429) }))
	defer srv.Close()
	c := New()
	c.Base = srv.URL
	_, err := c.Lookup(context.Background(), domain.Metadata{Title: "Song", Artist: "Artist"})
	e, ok := err.(requestError)

	if !ok || !e.Temporary() || e.RetryDelay() != 12*time.Second {
		t.Fatal(err)
	}
}
