package app_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kristakoch/break-up-playlist/internal/app"
	"github.com/kristakoch/break-up-playlist/internal/domain"
	"github.com/kristakoch/break-up-playlist/internal/sqlite"
)

type model struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
	fail    error
}

func (m *model) Identity(context.Context) (string, error) { return "model-digest", nil }

func (m *model) Classify(ctx context.Context, r domain.ChoiceRequest) (domain.Prediction, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()

	if m.entered != nil {
		m.entered <- struct{}{}

		select {
		case <-m.release:
		case <-ctx.Done():
			return domain.Prediction{}, ctx.Err()
		}
	}

	if m.fail != nil {
		return domain.Prediction{}, m.fail
	}

	probs := map[string]float64{}

	for i, c := range r.Categories {
		if i == 0 {
			probs[c.ID] = 1
		} else {
			probs[c.ID] = 0
		}
	}

	return domain.Prediction{Category: r.Categories[0].ID, Probabilities: probs}, nil
}

type lyrics struct {
	entered chan struct{}
	release chan struct{}
}

func (l lyrics) Lookup(ctx context.Context, m domain.Metadata) (app.LyricResult, error) {
	if l.entered != nil {
		l.entered <- struct{}{}

		select {
		case <-l.release:
		case <-ctx.Done():
			return app.LyricResult{}, ctx.Err()
		}
	}

	return app.LyricResult{Lyrics: "Provider lyrics", Status: "found", Source: "test"}, nil
}

type music struct {
	tracks        []domain.Metadata
	lyrics        map[string]string
	mu            sync.Mutex
	creates, adds int
	failAt        int
	source        bool
	keys          []string
}

func (m *music) Playlists(context.Context) ([]app.Playlist, error) { return nil, nil }

func (m *music) Import(context.Context, string) (string, []app.ImportedTrack, error) {
	tracks := make([]app.ImportedTrack, 0, len(m.tracks))
	for _, track := range m.tracks {
		tracks = append(tracks, app.ImportedTrack{Metadata: track, Lyrics: m.lyrics[track.ID]})
	}
	return "Source", tracks, nil
}

func (m *music) Create(ctx context.Context, name, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.creates++
	m.keys = append(m.keys, key)

	if m.source {
		return "source", nil
	}

	return fmt.Sprintf("destination-%d", m.creates), nil
}

func (m *music) Add(ctx context.Context, id string, ids []string, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.adds++
	m.keys = append(m.keys, key)

	if m.adds == m.failAt {
		return errors.New("network lost")
	}

	return nil
}

func setup(t *testing.T, m app.Classifier, l app.Lyrics, tide app.Music) (*app.Service, *sqlite.Store) {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	svc := app.New(ctx, store, m, l, tide)
	t.Cleanup(func() { cancel(); svc.Wait(); store.Close() })

	return svc, store
}

func collection() domain.Collection {
	return domain.Collection{ID: "c", Name: "Source", SourceID: "source", Categories: []domain.Category{{ID: "a", Name: "A", Description: "First"}, {ID: "b", Name: "B", Description: "Second"}}, Songs: []domain.Song{{Metadata: domain.Metadata{ID: "1", Title: "Song", Artist: "Artist", Album: "Album"}, Description: "Evidence", Manual: "b", Reference: "a"}}}
}

func wait(t *testing.T, s *sqlite.Store) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		c, err := s.Get(context.Background(), "c")
		if err != nil {
			t.Fatal(err)
		}

		if c.Job.State == "completed" || c.Job.State == "paused" || c.Job.State == "stopped" {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal("job did not finish")
}

func TestClassificationCacheResetAndPersistence(t *testing.T) {
	m := &model{}
	svc, store := setup(t, m, lyrics{}, &music{})
	c := collection()
	c.Songs = append(c.Songs, domain.Song{Metadata: domain.Metadata{ID: "no-evidence"}})
	if err := store.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}

	if err := svc.Start("c", "classification", false, false); err != nil {
		t.Fatal(err)
	}

	wait(t, store)
	svc.Wait()
	c, _ = store.Get(context.Background(), "c")

	if c.Songs[0].Prediction == nil || c.Songs[0].Manual != "b" || c.Songs[1].Prediction != nil {
		t.Fatal("classification violated evidence gate or assignment preservation")
	}

	if err := svc.Start("c", "classification", false, false); err != nil {
		t.Fatal(err)
	}

	svc.Wait()
	c, _ = store.Get(context.Background(), "c")

	if c.Job.Cached != 1 || m.calls != 1 {
		t.Fatalf("cache not reused %+v calls %d", c.Job, m.calls)
	}

	if err := svc.Reset(context.Background(), "c", false); err != nil {
		t.Fatal(err)
	}

	c, _ = store.Get(context.Background(), "c")

	if c.Songs[0].Prediction != nil || c.Songs[0].Manual != "b" || c.Songs[0].Reference != "a" || c.Songs[0].Description != "Evidence" {
		t.Fatal("reset lost human state")
	}

	if err := svc.Start("c", "classification", false, false); err != nil {
		t.Fatal(err)
	}

	svc.Wait()

	if m.calls != 2 {
		t.Fatal("reset reused cache")
	}
}

func TestManualLyricsWinInflightLookup(t *testing.T) {
	l := lyrics{entered: make(chan struct{}, 1), release: make(chan struct{})}
	svc, store := setup(t, &model{}, l, &music{})
	c := collection()
	c.Songs[0].LyricStatus = "unchecked"
	store.Save(context.Background(), c)

	if err := svc.Start("c", "lyrics", false, false); err != nil {
		t.Fatal(err)
	}

	<-l.entered

	if err := svc.EditSong(context.Background(), "c", "1", "My lyrics", "Evidence", "b", "a"); err != nil {
		t.Fatal(err)
	}

	close(l.release)
	svc.Wait()
	c, _ = store.Get(context.Background(), "c")

	if c.Songs[0].Lyrics != "My lyrics" || c.Songs[0].LyricStatus != "manual" {
		t.Fatal("lookup overwrote manual edit")
	}
}

func TestEvidenceChangeDiscardsInflightPrediction(t *testing.T) {
	m := &model{entered: make(chan struct{}, 1), release: make(chan struct{})}
	svc, store := setup(t, m, lyrics{}, &music{})
	store.Save(context.Background(), collection())
	svc.Start("c", "classification", false, false)
	<-m.entered
	svc.EditSong(context.Background(), "c", "1", "", "Changed evidence", "b", "a")
	close(m.release)
	svc.Wait()
	c, _ := store.Get(context.Background(), "c")

	if c.Songs[0].Prediction != nil {
		t.Fatal("saved stale prediction")
	}
}

func TestImportRetainsStateDeduplicatesAndInvalidates(t *testing.T) {
	c := collection()
	c.Songs[0].Lyrics = "Human lyrics"
	c.Songs[0].Prediction = &domain.Prediction{Category: "a"}
	m := &music{tracks: []domain.Metadata{c.Songs[0].Metadata, c.Songs[0].Metadata, {ID: "2"}}}
	svc, store := setup(t, &model{}, lyrics{}, m)
	store.Save(context.Background(), c)
	cid, err := svc.Import(context.Background(), "source")
	if err != nil || cid != "c" {
		t.Fatal(cid, err)
	}

	c, _ = store.Get(context.Background(), "c")

	if len(c.Songs) != 2 || c.Songs[0].Prediction == nil || c.Songs[0].Lyrics != "Human lyrics" || c.Songs[0].Manual != "b" {
		t.Fatal("reimport lost retained state")
	}

	m.tracks[0].Title = "Updated"
	svc.Import(context.Background(), "source")
	c, _ = store.Get(context.Background(), "c")

	if c.Songs[0].Prediction != nil || c.Songs[0].Lyrics != "Human lyrics" {
		t.Fatal("metadata invalidation failed")
	}
}

func TestExportSnapshotResumeAndSourceProtection(t *testing.T) {
	m := &music{failAt: 2}
	svc, store := setup(t, &model{}, lyrics{}, m)
	c := collection()
	c.Songs = nil

	for i := 0; i < 51; i++ {
		c.Songs = append(c.Songs, domain.Song{Metadata: domain.Metadata{ID: fmt.Sprint(i)}, Manual: "a"})
	}

	store.Save(context.Background(), c)

	if err := svc.Preview(context.Background(), "c"); err != nil {
		t.Fatal(err)
	}

	if m.creates != 0 {
		t.Fatal("preview wrote to provider")
	}

	c, _ = store.Get(context.Background(), "c")
	eid := c.Exports[0].ID
	store.Update(context.Background(), "c", func(c *domain.Collection) error { c.Revision++; return nil })

	if err := svc.Confirm(context.Background(), "c", eid); err == nil {
		t.Fatal("stale preview accepted")
	}

	svc.Preview(context.Background(), "c")
	c, _ = store.Get(context.Background(), "c")
	if err := svc.Confirm(context.Background(), "c", c.Exports[0].ID); err != nil {
		t.Fatal(err)
	}

	svc.Start("c", "export", false, false)
	svc.Wait()
	c, _ = store.Get(context.Background(), "c")

	if c.Job.State != "paused" || !c.Exports[0].Playlists[0].Batches[0].Done {
		t.Fatal("partial checkpoint missing")
	}

	failedKey := m.keys[len(m.keys)-1]
	store.Update(context.Background(), "c", func(c *domain.Collection) error { c.Songs[0].Manual = "b"; c.Revision++; return nil })
	svc.Start("c", "export", false, false)
	svc.Wait()
	c, _ = store.Get(context.Background(), "c")

	if m.creates != 1 || m.adds != 3 || c.Exports[0].State != "completed" || failedKey != m.keys[len(m.keys)-1] {
		t.Fatalf("unsafe resume: creates=%d adds=%d state=%s", m.creates, m.adds, c.Exports[0].State)
	}

	m.source = true
	svc.Preview(context.Background(), "c")
	c, _ = store.Get(context.Background(), "c")
	svc.Confirm(context.Background(), "c", c.Exports[len(c.Exports)-1].ID)
	before := m.adds
	svc.Start("c", "export", false, false)
	svc.Wait()

	if m.adds != before {
		t.Fatal("source playlist was mutated")
	}
}

func TestReferencedCategoryCannotBeRemoved(t *testing.T) {
	svc, store := setup(t, &model{}, lyrics{}, &music{})
	c := collection()
	store.Save(context.Background(), c)

	if err := svc.Category(context.Background(), "c", "a", "", "", true); err == nil {
		t.Fatal("reference label category removed")
	}

	if err := svc.Category(context.Background(), "c", "b", "", "", true); err == nil {
		t.Fatal("manual category removed")
	}
}

type transient struct{}

func (transient) Error() string { return "unavailable" }

func (transient) Temporary() bool { return true }

func TestStopInterruptsRetryDelay(t *testing.T) {
	m := &model{fail: transient{}}
	svc, store := setup(t, m, lyrics{}, &music{})
	store.Save(context.Background(), collection())
	svc.Start("c", "classification", false, false)
	deadline := time.Now().Add(time.Second)

	for time.Now().Before(deadline) {
		m.mu.Lock()
		n := m.calls
		m.mu.Unlock()

		if n > 0 {
			break
		}

		time.Sleep(time.Millisecond)
	}

	start := time.Now()
	svc.Stop("c")
	svc.Wait()

	if time.Since(start) > time.Second {
		t.Fatal("stop did not interrupt retry wait")
	}

	c, _ := store.Get(context.Background(), "c")

	if c.Job.State != "stopped" {
		t.Fatal(c.Job)
	}
}

func TestInvalidOutputDoesNotAbortRemainingSongs(t *testing.T) {
	m := &model{fail: app.ErrInvalidOutput}
	svc, store := setup(t, m, lyrics{}, &music{})
	c := collection()
	c.Songs = append(c.Songs, domain.Song{Metadata: domain.Metadata{ID: "2"}, Description: "Evidence"})
	if err := store.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}

	if err := svc.Start("c", "classification", false, false); err != nil {
		t.Fatal(err)
	}

	svc.Wait()
	c, _ = store.Get(context.Background(), "c")

	if c.Job.State != "completed" || c.Job.Failed != 2 || m.calls != 2 || c.Songs[0].Error == "" {
		t.Fatalf("invalid output not recorded per song: %+v", c.Job)
	}
}
func TestCategoryChangesKeepPredictionAndManual(t *testing.T) {
	svc, store := setup(t, &model{}, lyrics{}, &music{})
	c := collection()
	c.Songs[0].Prediction = &domain.Prediction{Category: "a", Probabilities: map[string]float64{"a": 1, "b": 0}, Version: domain.InstructionVersion}
	store.Save(context.Background(), c)

	if err := svc.Category(context.Background(), "c", "a", "A", "Revised criterion", false); err != nil {
		t.Fatal(err)
	}

	c, _ = store.Get(context.Background(), "c")

	if c.Songs[0].Prediction == nil || !c.Songs[0].Prediction.Review || c.Songs[0].Manual != "b" {
		t.Fatal("category edit discarded results")
	}

	if err := svc.Category(context.Background(), "c", "a", "B", "Duplicate", false); err == nil {
		t.Fatal("duplicate category accepted")
	}
}
func TestRecoveryMarksOldContractsAndPausesJobs(t *testing.T) {
	svc, store := setup(t, &model{}, lyrics{}, &music{})
	c := collection()
	c.Job = domain.Job{Kind: "classification", State: "running", Processed: 1}
	c.Songs[0].Prediction = &domain.Prediction{Category: "a", Version: "old-contract", Probabilities: map[string]float64{"a": 1, "b": 0}}
	store.Save(context.Background(), c)

	if err := svc.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}

	c, _ = store.Get(context.Background(), "c")

	if c.Job.State != "paused" || c.Job.Processed != 1 || c.Songs[0].Prediction == nil || !c.Songs[0].Prediction.Review {
		t.Fatal("restart lost or concealed old results")
	}
}
func TestStopSuppressesAutoLyricsUntilReopen(t *testing.T) {
	l := lyrics{entered: make(chan struct{}, 3), release: make(chan struct{})}
	svc, store := setup(t, &model{}, l, &music{})
	c := collection()
	c.Songs[0].LyricStatus = "unchecked"
	store.Save(context.Background(), c)
	svc.AutoEvidence("c")
	<-l.entered
	svc.Stop("c")
	svc.Wait()
	svc.AutoEvidence("c")

	select {
	case <-l.entered:
		t.Fatal("stopped lookup restarted")
	default:
	}

	svc.Leave("c")
	svc.AutoEvidence("c")

	select {
	case <-l.entered:
	case <-time.After(time.Second):
		t.Fatal("reopened lookup did not resume")
	}

	close(l.release)
	svc.Wait()
}

// A lost checkpoint simulates a crash after a provider accepted a mutation.
type checkpointStore struct {
	app.Store
	mu       sync.Mutex
	armed    bool
	afterAdd bool
}

func (s *checkpointStore) Update(ctx context.Context, id string, fn func(*domain.Collection) error) error {
	return s.Store.Update(ctx, id, func(c *domain.Collection) error {
		if err := fn(c); err != nil {
			return err
		}

		s.mu.Lock()
		defer s.mu.Unlock()

		if s.armed {
			for _, e := range c.Exports {
				for _, p := range e.Playlists {
					hit := p.Destination != ""

					if s.afterAdd {
						hit = len(p.Batches) > 0 && p.Batches[0].Done
					}

					if hit {
						s.armed = false

						return errors.New("checkpoint unavailable after provider success")
					}
				}
			}
		}

		return nil
	})
}

type idempotentMusic struct {
	music
	createKeys map[string]string
	addKeys    map[string]bool
}

func (m *idempotentMusic) Create(ctx context.Context, name, key string) (string, error) {
	if id := m.createKeys[key]; id != "" {
		return id, nil
	}

	id, err := m.music.Create(ctx, name, key)
	if err == nil {
		m.createKeys[key] = id
	}

	return id, err
}
func (m *idempotentMusic) Add(ctx context.Context, id string, ids []string, key string) error {
	if m.addKeys[key] {
		return nil
	}

	if err := m.music.Add(ctx, id, ids, key); err != nil {
		return err
	}

	m.addKeys[key] = true

	return nil
}
func TestProviderSuccessBeforeCheckpointIsResumable(t *testing.T) {
	for _, afterAdd := range []bool{false, true} {
		t.Run(fmt.Sprintf("after-add-%v", afterAdd), func(t *testing.T) {
			m := &idempotentMusic{createKeys: map[string]string{}, addKeys: map[string]bool{}}
			svc, store := setup(t, &model{}, lyrics{}, m)
			c := collection()
			if err := store.Save(context.Background(), c); err != nil {
				t.Fatal(err)
			}

			svc.Preview(context.Background(), "c")
			c, _ = store.Get(context.Background(), "c")
			svc.Confirm(context.Background(), "c", c.Exports[0].ID)
			svc.Store = &checkpointStore{Store: store, armed: true, afterAdd: afterAdd}
			if err := svc.Start("c", "export", false, false); err != nil {
				t.Fatal(err)
			}

			svc.Wait()
			c, _ = store.Get(context.Background(), "c")

			if c.Job.State != "paused" {
				t.Fatal("checkpoint failure not surfaced")
			}

			if err := svc.Start("c", "export", false, false); err != nil {
				t.Fatal(err)
			}

			svc.Wait()
			c, _ = store.Get(context.Background(), "c")

			if c.Exports[0].State != "completed" || m.creates != 1 || m.adds != 1 {
				t.Fatalf("duplicated remote work: creates=%d adds=%d state=%s", m.creates, m.adds, c.Exports[0].State)
			}
		})
	}
}

func TestRevertedCategoriesCanReuseReviewedPrediction(t *testing.T) {
	m := &model{}
	svc, store := setup(t, m, lyrics{}, &music{})
	store.Save(context.Background(), collection())
	svc.Start("c", "classification", false, false)
	svc.Wait()
	svc.Category(context.Background(), "c", "a", "A", "New definition", false)
	svc.Category(context.Background(), "c", "a", "A", "First", false)
	svc.Start("c", "classification", false, false)
	svc.Wait()
	c, _ := store.Get(context.Background(), "c")

	if c.Songs[0].Prediction == nil || c.Songs[0].Prediction.Review || c.Job.Cached != 1 || m.calls != 1 {
		t.Fatal("compatible cached prediction remained excluded from evaluation")
	}
}

func TestClassifyRemainingSkipsExistingPredictions(t *testing.T) {
	m := &model{}
	svc, store := setup(t, m, lyrics{}, &music{})
	c := collection()
	c.Songs[0].Prediction = &domain.Prediction{Category: "a", Fingerprint: "older-model", Review: true}
	c.Songs = append(c.Songs, domain.Song{Metadata: domain.Metadata{ID: "new"}, Description: "New evidence", Manual: "b"})
	c.Songs = append(c.Songs, domain.Song{Metadata: domain.Metadata{ID: "none"}})
	if err := store.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start("c", "classification", true, false); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	got, err := store.Get(context.Background(), "c")
	if err != nil {
		t.Fatal(err)
	}
	if m.calls != 1 || got.Job.Total != 1 || got.Job.Processed != 1 {
		t.Fatalf("unexpected job: calls=%d job=%+v", m.calls, got.Job)
	}
	if got.Song("1").Prediction.Fingerprint != "older-model" || got.Song("new").Prediction == nil || got.Song("new").Manual != "b" || got.Song("none").Prediction != nil {
		t.Fatal("remaining classification changed the wrong songs")
	}
}

func TestImportUsesTIDALLyricsWithoutOverwritingManualLyrics(t *testing.T) {
	existing := domain.Collection{ID: "c", Name: "Source", SourceID: "source", Songs: []domain.Song{
		{Metadata: domain.Metadata{ID: "manual", Title: "Manual"}, Lyrics: "My lyrics", LyricSource: "Manual entry", LyricStatus: "manual", Position: 1},
		{Metadata: domain.Metadata{ID: "old", Title: "Old"}, Lyrics: "Old external lyrics", LyricSource: "https://lrclib.net/record", LyricStatus: "found", Prediction: &domain.Prediction{Category: "a"}, Position: 2},
	}}
	m := &music{tracks: []domain.Metadata{existing.Songs[0].Metadata, existing.Songs[1].Metadata, {ID: "new", Title: "New"}}, lyrics: map[string]string{"manual": "TIDAL first", "old": "TIDAL second", "new": "TIDAL third"}}
	svc, store := setup(t, &model{}, lyrics{}, m)
	if err := store.Save(context.Background(), existing); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Import(context.Background(), "source"); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), "c")
	if err != nil {
		t.Fatal(err)
	}
	if got.Song("manual").Lyrics != "My lyrics" || got.Song("manual").LyricSource != "Manual entry" {
		t.Fatal("manual lyrics overwritten")
	}
	if got.Song("old").Lyrics != "TIDAL second" || got.Song("old").LyricSource != "TIDAL" || got.Song("old").Prediction != nil {
		t.Fatal("TIDAL lyrics did not replace old external evidence safely")
	}
	if got.Song("new").Lyrics != "TIDAL third" || !got.Song("new").Eligible() {
		t.Fatal("included TIDAL lyrics were not imported")
	}
}

type resultLyrics struct {
	calls  int
	result app.LyricResult
}

func (l *resultLyrics) Lookup(ctx context.Context, metadata domain.Metadata) (app.LyricResult, error) {
	l.calls++
	return l.result, nil
}

func TestFallbackLyricsRetriedWithoutRepeatingLRCLIB(t *testing.T) {
	primary := &resultLyrics{result: app.LyricResult{Status: "missing"}}
	fallback := &resultLyrics{result: app.LyricResult{Lyrics: "Possible lyrics", Source: "Lyrics.ovh", Status: "found"}}
	svc, store := setup(t, &model{}, primary, &music{})
	svc.FallbackLyrics = fallback
	c := collection()
	c.Songs[0].Lyrics = ""
	c.Songs[0].Description = ""
	c.Songs[0].LyricsSearchVersion = 1
	c.Songs[0].LyricStatus = "missing"
	c.Songs[0].LyricsOVHStatus = "failed"
	if err := store.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start("c", "lyrics", true, false); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	c, _ = store.Get(context.Background(), "c")
	if primary.calls != 0 || fallback.calls != 1 || c.Songs[0].Lyrics != "Possible lyrics" || c.Songs[0].LyricSource != "Lyrics.ovh" || !c.Songs[0].Eligible() {
		t.Fatalf("unexpected retry: primary=%d fallback=%d song=%+v", primary.calls, fallback.calls, c.Songs[0])
	}
}

type unavailableLyrics struct{}

func (unavailableLyrics) Lookup(context.Context, domain.Metadata) (app.LyricResult, error) {
	return app.LyricResult{}, errors.New("provider unavailable")
}

func TestFallbackOutageDoesNotStopPrimaryLookup(t *testing.T) {
	primary := &resultLyrics{result: app.LyricResult{Status: "missing"}}
	svc, store := setup(t, &model{}, primary, &music{})
	svc.FallbackLyrics = unavailableLyrics{}
	c := collection()
	c.Songs[0].Lyrics = ""
	c.Songs[0].LyricStatus = "unchecked"
	c.Songs = append(c.Songs, domain.Song{Metadata: domain.Metadata{ID: "second"}, LyricStatus: "unchecked"})
	if err := store.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start("c", "lyrics", false, false); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	c, _ = store.Get(context.Background(), "c")
	if primary.calls != 2 || c.Song("1").LyricsOVHStatus != "failed" || c.Song("second").LyricStatus != "missing" || c.Job.State != "completed" {
		t.Fatalf("fallback failure stopped primary lookup: calls=%d job=%+v", primary.calls, c.Job)
	}
}

type signaledMissingLyrics struct {
	calls chan string
}

func (l signaledMissingLyrics) Lookup(ctx context.Context, metadata domain.Metadata) (app.LyricResult, error) {
	l.calls <- metadata.ID
	return app.LyricResult{Status: "missing"}, nil
}

type blockingFallbackLyrics struct {
	entered chan string
}

func (l blockingFallbackLyrics) Lookup(ctx context.Context, metadata domain.Metadata) (app.LyricResult, error) {
	l.entered <- metadata.ID
	<-ctx.Done()
	return app.LyricResult{}, ctx.Err()
}

func TestLyricsFallbackDoesNotBlockPrimaryAndStopCancelsBoth(t *testing.T) {
	primary := signaledMissingLyrics{calls: make(chan string, 2)}
	fallback := blockingFallbackLyrics{entered: make(chan string, 2)}
	svc, store := setup(t, &model{}, primary, &music{})
	svc.FallbackLyrics = fallback
	c := collection()
	c.Songs[0].LyricStatus = "unchecked"
	c.Songs = append(c.Songs, domain.Song{Metadata: domain.Metadata{ID: "second"}, LyricStatus: "unchecked"})
	if err := store.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start("c", "lyrics", false, false); err != nil {
		t.Fatal(err)
	}

	select {
	case <-fallback.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("fallback did not start")
	}

	for i := 0; i < 2; i++ {
		select {
		case <-primary.calls:
		case <-time.After(3 * time.Second):
			t.Fatal("primary lookup stopped behind the fallback")
		}
	}

	select {
	case <-fallback.entered:
		t.Fatal("fallback requests overlapped")
	default:
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		current, err := store.Get(context.Background(), "c")
		if err == nil && current.Song("second").LyricStatus == "missing" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second primary result was not saved")
		}
		time.Sleep(5 * time.Millisecond)
	}

	svc.Stop("c")
	svc.Wait()
	c, _ = store.Get(context.Background(), "c")
	if c.Job.State != "stopped" || c.Song("second").LyricStatus != "missing" {
		t.Fatalf("stop did not preserve primary progress: %+v", c.Job)
	}
}

type unavailableExportMusic struct {
	*music
	lastKey string
	keys    []string
}

func (m *unavailableExportMusic) Add(ctx context.Context, id string, ids []string, key string) error {
	m.lastKey = key
	m.keys = append(m.keys, key)
	for _, trackID := range ids {
		if trackID == "unavailable" {
			return &domain.UnavailableTracksError{IDs: []string{trackID}}
		}
	}
	return nil
}

func TestExportAutomaticallyOmitsUnavailableTracks(t *testing.T) {
	provider := &unavailableExportMusic{music: &music{}}
	svc, store := setup(t, &model{}, lyrics{}, provider)
	c := collection()
	c.Songs[0].Manual = "a"
	c.Songs = append(c.Songs, domain.Song{Metadata: domain.Metadata{ID: "unavailable", Title: "Unavailable"}, Manual: "a"})
	if err := store.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := svc.Preview(context.Background(), "c"); err != nil {
		t.Fatal(err)
	}
	c, _ = store.Get(context.Background(), "c")
	if c.Exports[0].Playlists[0].Name != "A" {
		t.Fatal("export playlist name should be the category name")
	}
	exportID := c.Exports[0].ID
	if err := svc.Confirm(context.Background(), "c", exportID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start("c", "export", false, false); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	c, _ = store.Get(context.Background(), "c")
	if c.Exports[0].State != "completed" || len(c.Exports[0].Omitted) != 1 || c.Exports[0].Omitted[0].ID != "unavailable" {
		t.Fatalf("unavailable track was not recorded: %+v", c.Exports[0])
	}
	if len(provider.keys) != 2 || provider.keys[0] == provider.keys[1] {
		t.Fatal("changed batch did not receive a new idempotency key")
	}
	if len(c.Exports[0].Playlists[0].Tracks) != 2 || len(c.Exports[0].Playlists[0].Batches[0].IDs) != 1 {
		t.Fatal("confirmed snapshot was lost or failed batch was not adjusted")
	}
}

func TestRemoveExportHistoryOnlyRemovesCompletedRecords(t *testing.T) {
	svc, store := setup(t, &model{}, lyrics{}, &music{})
	c := collection()
	c.Exports = []domain.Export{
		{ID: "completed", Confirmed: true, State: "completed", Playlists: []domain.ExportPlaylist{{Destination: "existing-tidal-playlist"}}},
		{ID: "paused", Confirmed: true, State: "paused"},
		{ID: "preview", State: "preview"},
	}
	if err := store.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}

	if err := svc.RemoveExportHistory(context.Background(), "c", "paused"); err == nil {
		t.Fatal("removed an unfinished export")
	}
	if err := svc.RemoveExportHistory(context.Background(), "c", "preview"); err == nil {
		t.Fatal("removed a preview")
	}
	if err := svc.RemoveExportHistory(context.Background(), "c", "completed"); err != nil {
		t.Fatal(err)
	}
	c, _ = store.Get(context.Background(), "c")
	if len(c.Exports) != 2 || c.Exports[0].ID != "paused" || c.Exports[1].ID != "preview" {
		t.Fatal("removed the wrong export record")
	}
}
