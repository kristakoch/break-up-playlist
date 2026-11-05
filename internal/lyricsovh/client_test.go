package lyricsovh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kristakoch/break-up-playlist/internal/domain"
)

func TestLyricsAndMissingAreDistinct(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/Artist/Song" {
			_, _ = w.Write([]byte(`{"lyrics":"A possible match"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := New()
	client.Base = server.URL
	got, err := client.Lookup(context.Background(), domain.Metadata{Artist: "Artist", Title: "Song"})
	if err != nil || got.Lyrics != "A possible match" || got.Source != "Lyrics.ovh" {
		t.Fatalf("unexpected result: %+v, %v", got, err)
	}
	missing, err := client.Lookup(context.Background(), domain.Metadata{Artist: "Artist", Title: "Missing"})
	if err != nil || missing.Status != "missing" {
		t.Fatalf("unexpected missing result: %+v, %v", missing, err)
	}
}
