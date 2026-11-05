package openjev

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kristakoch/break-up-playlist/internal/domain"
)

func TestChoiceContractAndPrivacy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"models":[{"name":"openjev-latest"}]}`)

			return
		}

		if r.URL.Path != "/v1/systemone" {
			t.Errorf("unexpected path %s", r.URL.Path)

			return
		}

		var body struct {
			Model, State string
			Questions    map[string]struct {
				Type     string
				Criteria map[string]string
			}
		}

		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}

		if body.Model != "openjev-latest" || body.Questions["category"].Type != "choice" || body.Questions["category"].Criteria["a"] != "Calm: Quiet" {
			t.Error("invalid choice request")
		}

		if strings.Contains(body.State, "secret-human") {
			t.Error("human label leaked")
		}

		var evidence domain.Evidence
		if err := json.Unmarshal([]byte(body.State), &evidence); err != nil {
			t.Fatal(err)
		}
		if evidence.Metadata.BPM == nil || *evidence.Metadata.BPM != 120.5 || evidence.Metadata.Key != "CSharp" || evidence.Metadata.KeyScale != "MINOR" {
			t.Errorf("audio metadata missing from OpenJev request: %+v", evidence.Metadata)
		}

		fmt.Fprint(w, `{"answers":{"category":{"choice":"a","probabilities":{"a":0.7,"b":0.3},"confidence":0.01}}}`)
	}))
	defer srv.Close()
	c, err := New(srv.URL, "openjev-latest", "mlx", "weights", "")
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.Identity(context.Background()); err != nil {
		t.Fatal(err)
	}

	bpm := 120.5
	song := domain.Song{Metadata: domain.Metadata{BPM: &bpm, Key: "CSharp", KeyScale: "MINOR"}, Description: "Evidence", Reference: "secret-human", Manual: "secret-human"}
	cats := []domain.Category{{ID: "a", Name: "Calm", Description: "Quiet"}, {ID: "b", Name: "Energetic", Description: "Fast"}}
	p, err := c.Classify(context.Background(), domain.ChoiceRequest{Evidence: song.Evidence(), Categories: cats})
	if err != nil {
		t.Fatal(err)
	}

	if err = domain.ValidatePrediction(p, cats); err != nil || p.Category != "a" {
		t.Fatal(p, err)
	}
}

func TestRejectRemoteEndpoint(t *testing.T) {
	for _, u := range []string{"https://example.com", "http://192.168.1.1:8080", "http://localhost.evil.example"} {
		if _, err := New(u, "x", "mlx", "x", ""); err == nil {
			t.Fatalf("accepted %s", u)
		}
	}
}

// Set OPENJEV_TEST_URL to run an optional test against an already-running local server.
func TestLocalOpenJevIntegration(t *testing.T) {
	base := os.Getenv("OPENJEV_TEST_URL")

	if base == "" {
		t.Skip("no local OpenJev integration server configured")
	}

	c, err := New(base, "openjev-latest", "mlx", "mlx-community/diffusiongemma-26B-A4B-it-4bit", "")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if _, err = c.Identity(ctx); err != nil {
		t.Fatal(err)
	}

	categories := []domain.Category{{ID: "a", Name: "Calm", Description: "Quiet and reflective"}, {ID: "b", Name: "Energetic", Description: "Fast and driving"}}
	p, err := c.Classify(ctx, domain.ChoiceRequest{Evidence: domain.Evidence{Metadata: domain.Metadata{Title: "Test track", Artist: "Fictional artist"}, Description: "Soft piano and a slow, quiet melody."}, Categories: categories})
	if err != nil {
		t.Fatal(err)
	}

	if err = domain.ValidatePrediction(p, categories); err != nil {
		t.Fatal(err)
	}
}

func TestLocalChoiceCounts(t *testing.T) {
	base := os.Getenv("OPENJEV_TEST_URL")

	if base == "" {
		t.Skip("no local OpenJev integration server configured")
	}

	c, err := New(base, "openjev-latest", "mlx", "mlx-community/diffusiongemma-26B-A4B-it-4bit", "")
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range []int{1, 20} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			cats := make([]domain.Category, n)

			for i := range cats {
				cats[i] = domain.Category{ID: fmt.Sprintf("c%d", i), Name: fmt.Sprintf("Category %d", i), Description: fmt.Sprintf("A description for category %d", i)}
			}

			p, err := c.Classify(context.Background(), domain.ChoiceRequest{Evidence: domain.Evidence{Description: "Quiet piano"}, Categories: cats})
			if err != nil {
				t.Fatal(err)
			}

			if err = domain.ValidatePrediction(p, cats); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIdentityTracksCachedWeightsRevision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"models":[{"name":"openjev-latest","release_date":"2026-09-18"}]}`)
	}))
	defer server.Close()
	client, err := New(server.URL, "openjev-latest", "mlx", "org/model", "")
	if err != nil {
		t.Fatal(err)
	}
	client.CacheDir = t.TempDir()
	refs := filepath.Join(client.CacheDir, "hub", "models--org--model", "refs")
	if err = os.MkdirAll(refs, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(refs, "main"), []byte("revision-one"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := client.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(refs, "main"), []byte("revision-two"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := client.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !strings.Contains(second, "revision-two") {
		t.Fatal("model weight revision did not affect identity")
	}
}
