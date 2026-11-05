// Package domain contains provider-independent types and classification rules.
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

const InstructionVersion = "choice-v2-openjev"

type Category struct{ ID, Name, Description string }
type Metadata struct {
	ID, Title, Artist, Album string
	Duration                 float64
	Catalog                  map[string]any
	BPM                      *float64 `json:"BPM,omitempty"`
	Key                      string   `json:"Key,omitempty"`
	KeyScale                 string   `json:"KeyScale,omitempty"`
}
type Prediction struct {
	Category                    string
	Probabilities               map[string]float64
	Model, Version, Fingerprint string
	Review                      bool
}
type Song struct {
	Metadata
	Position                                      int
	Lyrics, Description, LyricSource, LyricStatus string
	Manual, Reference                             string
	Prediction                                    *Prediction
	Error                                         string
	MusicBrainz                                   *MusicBrainzEvidence
	MusicBrainzStatus                             string
	LyricsSearchVersion                           int
	LyricsOVHStatus                               string
	LyricSuggestion                               *LyricSuggestion
}

// LyricSuggestion is a possible match awaiting the listener's approval.
type LyricSuggestion struct {
	Lyrics, Source string
}

// MusicBrainzEvidence is a reviewable provider suggestion, not user-approved evidence.
type MusicBrainzEvidence struct {
	RecordingID, SourceURL, MatchMethod string
	Facts                               []string
}
type Collection struct {
	ID, Name, SourceID string
	Revision           int
	Categories         []Category
	Songs              []Song
	Job                Job
	Exports            []Export
}
type Job struct {
	Kind, State, Message             string
	Processed, Total, Cached, Failed int
}
type ExportPlaylist struct {
	Category, Name, Destination, CreateKey string
	Tracks                                 []Metadata
	Batches                                []Batch
}
type Batch struct {
	Key  string
	IDs  []string
	Done bool
}
type Export struct {
	ID           string
	Revision     int
	Confirmed    bool
	State, Error string
	Playlists    []ExportPlaylist
	Unavailable  []Metadata
	Omitted      []Metadata
}

// UnavailableTracksError identifies tracks a provider could not add.
type UnavailableTracksError struct{ IDs []string }

func (e *UnavailableTracksError) Error() string {
	return fmt.Sprintf("TIDAL could not add %d unavailable track(s)", len(e.IDs))
}

// Evidence is an allowlist: human assignments and reference labels cannot leak into model requests.
type Evidence struct {
	Metadata            Metadata
	Lyrics, Description string
}
type ChoiceRequest struct {
	Evidence   Evidence
	Categories []Category
}

func (s Song) Evidence() Evidence { return Evidence{s.Metadata, s.Lyrics, s.Description} }

func (s Song) Eligible() bool {
	return strings.TrimSpace(s.Lyrics) != "" || strings.TrimSpace(s.Description) != ""
}

func (s Song) Incomplete() bool { return s.Title == "" || s.Artist == "" || s.Album == "" }

func (s Song) DisplayTitle() string {
	if s.Title == "" {
		return "Unknown title (track " + s.ID + ")"
	}

	return s.Title
}

func (s Song) Assigned() string {
	if s.Manual != "" {
		return s.Manual
	}

	if s.Prediction != nil {
		return s.Prediction.Category
	}

	return ""
}

func (s Song) Score() float64 {
	if s.Prediction == nil {
		return -1
	}

	return s.Prediction.Probabilities[s.Prediction.Category]
}

func (c Collection) CategoryName(id string) string {
	for _, v := range c.Categories {
		if v.ID == id {
			return v.Name
		}
	}

	return "Unassigned"
}

func (c Collection) HasCategory(id string) bool {
	for _, v := range c.Categories {
		if v.ID == id {
			return true
		}
	}

	return false
}

func (c Collection) Song(id string) *Song {
	for i := range c.Songs {
		if c.Songs[i].ID == id {
			return &c.Songs[i]
		}
	}

	return nil
}

func Fingerprint(s Song, cats []Category, model string) string {
	b, _ := json.Marshal(struct {
		Request        ChoiceRequest
		Model, Version string
	}{ChoiceRequest{s.Evidence(), cats}, model, InstructionVersion})
	h := sha256.Sum256(b)

	return hex.EncodeToString(h[:])
}

func ValidatePrediction(p Prediction, cats []Category) error {
	if len(cats) == 0 || len(p.Probabilities) != len(cats) {
		return errors.New("model distribution must contain every category exactly once")
	}

	sum, max := 0.0, -1.0
	valid := false

	for _, c := range cats {
		v, ok := p.Probabilities[c.ID]

		if !ok || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return fmt.Errorf("invalid probability for %s", c.Name)
		}

		sum += v

		if v > max {
			max = v
		}

		if c.ID == p.Category {
			valid = true
		}
	}

	if !valid || p.Probabilities[p.Category] != max {
		return errors.New("chosen category is not a highest-scoring choice")
	}

	if math.Abs(sum-1) > 0.01 {
		return errors.New("probabilities must sum to approximately one")
	}

	return nil
}

func ValidateCategories(cats []Category) error {
	if len(cats) > 20 {
		return errors.New("at most 20 categories are supported")
	}

	seen := map[string]bool{}

	for _, c := range cats {
		n := strings.ToLower(strings.TrimSpace(c.Name))

		if n == "" || strings.TrimSpace(c.Description) == "" {
			return errors.New("category name and description are required")
		}

		if seen[n] {
			return errors.New("category names must be unique")
		}

		seen[n] = true
	}

	return nil
}

func Referenced(c Collection, id string) bool {
	for _, s := range c.Songs {
		if s.Manual == id || s.Reference == id {
			return true
		}

		if s.Prediction != nil {
			if _, ok := s.Prediction.Probabilities[id]; ok {
				return true
			}
		}
	}

	return false
}

type Evaluation struct {
	Labels                                 []string
	Matrix                                 [][]int
	Labeled, Predicted, Evaluated, Correct int
	Coverage, Accuracy, MacroF1            float64
}

func Evaluate(c Collection) Evaluation {
	e := Evaluation{}
	indices := map[string]int{}

	for i, cat := range c.Categories {
		indices[cat.ID] = i
		e.Labels = append(e.Labels, cat.Name)
		e.Matrix = append(e.Matrix, make([]int, len(c.Categories)))
	}

	for _, s := range c.Songs {
		ri, labeled := indices[s.Reference]

		if labeled {
			e.Labeled++
		}

		if s.Prediction == nil || s.Prediction.Review {
			continue
		}

		pi, predicted := indices[s.Prediction.Category]

		if !predicted {
			continue
		}

		e.Predicted++

		if !labeled {
			continue
		}

		e.Evaluated++
		e.Matrix[ri][pi]++

		if ri == pi {
			e.Correct++
		}
	}

	if len(c.Songs) > 0 {
		e.Coverage = float64(e.Predicted) / float64(len(c.Songs))
	}

	if e.Evaluated > 0 {
		e.Accuracy = float64(e.Correct) / float64(e.Evaluated)
	}

	for i := range e.Labels {
		tp, fp, fn := e.Matrix[i][i], 0, 0

		for j := range e.Labels {
			if j != i {
				fp += e.Matrix[j][i]
				fn += e.Matrix[i][j]
			}
		}

		if d := 2*tp + fp + fn; d > 0 {
			e.MacroF1 += float64(2*tp) / float64(d)
		}
	}

	if len(e.Labels) > 0 {
		e.MacroF1 /= float64(len(e.Labels))
	}

	return e
}
