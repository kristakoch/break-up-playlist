// Package app implements workflows using interfaces owned by the application.
package app

import (
	"context"
	"errors"

	"github.com/kristakoch/break-up-playlist/internal/domain"
)

type Store interface {
	List(context.Context) ([]domain.Collection, error)
	Get(context.Context, string) (domain.Collection, error)
	Save(context.Context, domain.Collection) error
	Update(context.Context, string, func(*domain.Collection) error) error
	Delete(context.Context, string) error
}
type Classifier interface {
	Identity(context.Context) (string, error)
	Classify(context.Context, domain.ChoiceRequest) (domain.Prediction, error)
}
type LyricResult struct{ Lyrics, Source, Status string }
type Lyrics interface {
	Lookup(context.Context, domain.Metadata) (LyricResult, error)
}
type Playlist struct{ ID, Name string }
type ImportedTrack struct {
	domain.Metadata
	Lyrics string
}
type Music interface {
	Playlists(context.Context) ([]Playlist, error)
	Import(context.Context, string) (string, []ImportedTrack, error)
	Create(context.Context, string, string) (string, error)
	Add(context.Context, string, []string, string) error
}

// ErrInvalidOutput identifies a per-song model contract failure, rather than a service outage.
var ErrInvalidOutput = errors.New("invalid model output")
