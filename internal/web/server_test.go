package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kristakoch/break-up-playlist/internal/app"
	"github.com/kristakoch/break-up-playlist/internal/domain"
	"github.com/kristakoch/break-up-playlist/internal/sqlite"
)

type account struct{}

func (account) Connected() bool { return false }

func (account) CanWrite() bool { return false }

func (account) Authorization(bool) (string, string, error) { return "", "", nil }

func (account) Callback(context.Context, string, string) error { return nil }

func handler(t *testing.T) (http.Handler, *app.Service) {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	svc := app.New(ctx, store, nil, nil, nil)
	t.Cleanup(func() { cancel(); svc.Wait(); store.Close() })
	h, err := New(svc, account{}, "localhost:8080")
	if err != nil {
		t.Fatal(err)
	}

	return h, svc
}

func request(h http.Handler, method, path string, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost:8080"+path, strings.NewReader(form.Encode()))

	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	return w
}

func TestPagesFormsBlindModeAndSecurity(t *testing.T) {
	h, svc := handler(t)
	w := request(h, "GET", "/", nil)

	if w.Code != 200 || !strings.Contains(w.Body.String(), "Your collections") {
		t.Fatal(w.Code, w.Body.String())
	}

	body := w.Body.String()
	start := strings.Index(body, `name="csrf" value="`) + len(`name="csrf" value="`)
	token := strings.Split(body[start:], `"`)[0]
	c := domain.Collection{ID: "test", Name: "Test playlist", SourceID: "source", Categories: []domain.Category{{ID: "quiet", Name: "Quiet", Description: "Quiet songs"}}, Songs: []domain.Song{{Metadata: domain.Metadata{ID: "track-01", Title: "Test track"}, Description: "Evidence", LyricStatus: "missing"}}}
	if err := svc.Store.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}

	path := "/collections/test"
	w = request(h, "GET", path, nil)

	if w.Code != 200 || !strings.Contains(w.Body.String(), "Test track") {
		t.Fatal(w.Code, w.Body.String())
	}

	cid := strings.TrimPrefix(path, "/collections/")
	svc.Store.Update(context.Background(), cid, func(c *domain.Collection) error {
		c.Songs[0].Prediction = &domain.Prediction{Category: "quiet", Model: "SECRET-PREDICTION", Probabilities: map[string]float64{"quiet": 1}}

		return nil
	})

	for _, p := range []string{path + "?view=blind", path + "/songs/track-01?blind=1"} {
		w = request(h, "GET", p, nil)

		if w.Code != 200 || strings.Contains(w.Body.String(), "SECRET-PREDICTION") || strings.Contains(w.Body.String(), "<th>Model score</th>") {
			t.Fatal("blind view exposed prediction or failed", w.Code, w.Body.String())
		}
	}

	w = request(h, "GET", path+"/songs/track-01", nil)

	if w.Code != 200 || !strings.Contains(w.Body.String(), "SECRET-PREDICTION") {
		t.Fatal("details missing model")
	}

	w = request(h, "POST", path+"/songs/track-01", url.Values{"csrf": {token}, "description": {"<script>not executable</script>"}, "reference": {"quiet"}})

	if w.Code != 303 {
		t.Fatal(w.Code)
	}

	w = request(h, "GET", path+"/songs/track-01", nil)

	if strings.Contains(w.Body.String(), "<script>not executable") {
		t.Fatal("HTML injection")
	}

	if request(h, "POST", "/connect", url.Values{}).Code != 403 {
		t.Fatal("CSRF not enforced")
	}

	r := httptest.NewRequest("GET", "http://evil.example/", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != 403 {
		t.Fatal("host not enforced")
	}

	for _, p := range []string{path + "/status", path + "/report", path + "/report?format=csv"} {
		if w = request(h, "GET", p, nil); w.Code != 200 {
			t.Fatal(p, w.Code)
		}
	}
}

func TestFormOriginAndReferrerPolicy(t *testing.T) {
	h, _ := handler(t)
	home := request(h, "GET", "/", nil)

	if got := home.Header().Get("Referrer-Policy"); got != "same-origin" {
		t.Fatalf("referrer policy = %q", got)
	}

	body := home.Body.String()
	start := strings.Index(body, `name="csrf" value="`) + len(`name="csrf" value="`)
	token := strings.Split(body[start:], `"`)[0]

	for _, tc := range []struct {
		origin string
		want   int
	}{{"http://localhost:8080", http.StatusSeeOther}, {"null", http.StatusForbidden}, {"http://evil.example", http.StatusForbidden}} {
		form := url.Values{"csrf": {token}}
		r := httptest.NewRequest("POST", "http://localhost:8080/connect", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)

		if w.Code != tc.want {
			t.Errorf("origin %q: got %d, want %d", tc.origin, w.Code, tc.want)
		}
	}
}

func TestTIDALAuthorizationRedirectAllowedByFormPolicy(t *testing.T) {
	h, _ := handler(t)
	home := request(h, "GET", "/", nil)
	policy := home.Header().Get("Content-Security-Policy")

	if !strings.Contains(policy, "form-action 'self' https://login.tidal.com") {
		t.Fatalf("TIDAL authorization redirect is not allowed by form policy: %s", policy)
	}

	if strings.Contains(policy, "form-action *") || strings.Contains(policy, "form-action https:") {
		t.Fatal("form policy permits unrelated destinations")
	}
}

func TestDeleteCollectionRequiresConfirmation(t *testing.T) {
	h, svc := handler(t)
	ctx := context.Background()
	c := domain.Collection{ID: "delete-me", Name: "My playlist", SourceID: "tidal-source", Songs: []domain.Song{{Metadata: domain.Metadata{ID: "track"}, LyricStatus: "missing"}}}
	if err := svc.Store.Save(ctx, c); err != nil {
		t.Fatal(err)
	}

	page := request(h, "GET", "/collections/delete-me", nil)

	if !strings.Contains(page.Body.String(), "Delete this collection") || !strings.Contains(page.Body.String(), "Your TIDAL playlists are not changed") {
		t.Fatal("delete controls missing")
	}

	body := page.Body.String()
	start := strings.Index(body, `name="csrf" value="`) + len(`name="csrf" value="`)
	token := strings.Split(body[start:], `"`)[0]
	response := request(h, "POST", "/collections/delete-me/delete", url.Values{"csrf": {token}})

	if response.Code != http.StatusSeeOther {
		t.Fatal(response.Code)
	}

	if _, err := svc.Store.Get(ctx, c.ID); err != nil {
		t.Fatal("collection deleted without confirmation", err)
	}

	response = request(h, "POST", "/collections/delete-me/delete", url.Values{"csrf": {token}, "confirm": {"yes"}})

	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/" {
		t.Fatal(response.Code, response.Header().Get("Location"))
	}

	if _, err := svc.Store.Get(ctx, c.ID); err == nil {
		t.Fatal("confirmed deletion did not remove collection")
	}
}

func TestCollectionGroupsExpandTenAtATime(t *testing.T) {
	h, svc := handler(t)
	c := domain.Collection{ID: "pages", Name: "Playlist"}
	for i := 1; i <= 23; i++ {
		c.Songs = append(c.Songs, domain.Song{Metadata: domain.Metadata{ID: fmt.Sprintf("track-%02d", i), Title: fmt.Sprintf("Song %02d", i)}, Position: i, Description: "Evidence", LyricStatus: "missing"})
	}
	if err := svc.Store.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	first := request(h, "GET", "/collections/pages?sort=title", nil)
	if !strings.Contains(first.Body.String(), "Song 10") || strings.Contains(first.Body.String(), "Song 11") || !strings.Contains(first.Body.String(), "Show 10 more songs") {
		t.Fatal("first group did not show only ten songs")
	}
	next := request(h, "GET", "/collections/pages?sort=title&show_unassigned=20", nil)
	if !strings.Contains(next.Body.String(), "Song 20") || strings.Contains(next.Body.String(), "Song 21") || !strings.Contains(next.Body.String(), "Show fewer songs") || !strings.Contains(next.Body.String(), "sort=title") {
		t.Fatal("expanded group did not preserve sorting or show twenty songs")
	}
	last := request(h, "GET", "/collections/pages?show_unassigned=23", nil)
	if !strings.Contains(last.Body.String(), "Song 23") || strings.Contains(last.Body.String(), "Show 10 more songs") {
		t.Fatal("final expansion incorrect")
	}
}

func TestUnassignedOnlyIncludesEligibleSongs(t *testing.T) {
	h, svc := handler(t)
	c := domain.Collection{
		ID: "eligibility", Name: "Playlist",
		Categories: []domain.Category{{ID: "cat", Name: "Category", Description: "A category"}},
		Songs: []domain.Song{
			{Metadata: domain.Metadata{ID: "lyrics", Title: "Lyrics evidence"}, Lyrics: "Some lyrics", Position: 1},
			{Metadata: domain.Metadata{ID: "notes", Title: "Description evidence"}, Description: "A song description", Position: 2},
			{Metadata: domain.Metadata{ID: "none", Title: "No evidence"}, Lyrics: "  ", Description: "\n", Position: 3},
			{Metadata: domain.Metadata{ID: "manual", Title: "Manual without evidence"}, Manual: "cat", Position: 4},
		},
	}
	if err := svc.Store.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	page := request(h, "GET", "/collections/eligibility", nil).Body.String()
	start := strings.Index(page, `id="group-unassigned"`)
	end := strings.Index(page, `id="group-cat"`)
	if start < 0 || end <= start {
		t.Fatal("expected groups missing")
	}
	unassigned := page[start:end]
	if !strings.Contains(unassigned, `data-count="Ready"`) {
		t.Fatal("unassigned group count is not live")
	}
	categoryHeader := page[end:]
	categoryHeader = strings.SplitN(categoryHeader, "</h3>", 2)[0]
	if !strings.Contains(categoryHeader, `data-category-count="cat"`) {
		t.Fatal("category group count is not live")
	}
	filtered := request(h, "GET", "/collections/eligibility?q=evidence", nil).Body.String()
	filteredStart := strings.Index(filtered, `id="group-unassigned"`)
	if filteredStart < 0 {
		t.Fatal("filtered group missing")
	}
	filteredHeader := strings.SplitN(filtered[filteredStart:], "</h3>", 2)[0]
	if strings.Contains(filteredHeader, `data-count="Ready"`) {
		t.Fatal("filtered group would display unfiltered live count")
	}
	if !strings.Contains(unassigned, "Lyrics evidence") || !strings.Contains(unassigned, "Description evidence") || strings.Contains(unassigned, "No evidence") || strings.Contains(unassigned, "Manual without evidence") {
		t.Fatal("unassigned group contains wrong tracks")
	}
	if !strings.Contains(page, `id="group-missing-evidence"`) || !strings.Contains(page, "No evidence") {
		t.Fatal("ineligible song is not accessible")
	}
	needs := request(h, "GET", "/collections/eligibility?view=needs", nil).Body.String()
	if !strings.Contains(needs, "No evidence") || !strings.Contains(needs, "Manual without evidence") {
		t.Fatal("needs view omitted tracks requiring attention")
	}
}

func TestStatusIncludesLiveCounts(t *testing.T) {
	h, svc := handler(t)
	c := domain.Collection{ID: "counts", Name: "Playlist", Categories: []domain.Category{{ID: "a", Name: "A"}, {ID: "b", Name: "B"}}, Songs: []domain.Song{
		{Metadata: domain.Metadata{ID: "1", Title: "One", Artist: "Artist", Album: "Album"}, Description: "Evidence", Prediction: &domain.Prediction{Category: "a"}, Manual: "b", LyricStatus: "missing"},
		{Metadata: domain.Metadata{ID: "2", Title: "Two", Artist: "Artist", Album: "Album"}, Lyrics: "Lyrics", LyricStatus: "manual"},
		{Metadata: domain.Metadata{ID: "3"}, LyricStatus: "missing"},
	}}
	if err := svc.Store.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	w := request(h, "GET", "/collections/counts/status", nil)
	var result struct{ Counts collectionCounts }
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	got := result.Counts
	if got.Total != 3 || got.Assigned != 1 || got.Ready != 1 || got.Needs != 1 || got.Lyrics != 1 || got.Predictions != 1 || got.Categories["b"] != 1 || got.Categories["a"] != 0 {
		t.Fatalf("incorrect counts: %+v", got)
	}
}

func TestMissingEvidenceHasGoogleSearchLink(t *testing.T) {
	h, svc := handler(t)
	c := domain.Collection{ID: "search", Songs: []domain.Song{
		{Metadata: domain.Metadata{ID: "missing", Title: "A & B", Artist: "Artist + Guest"}},
		{Metadata: domain.Metadata{ID: "complete", Title: "Complete", Artist: "Artist"}, Lyrics: "lyrics"},
	}}
	if err := svc.Store.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}

	missing := request(h, "GET", "/collections/search/songs/missing", nil)
	if !strings.Contains(missing.Body.String(), "https://www.google.com/search?q=A%20%26%20B%20by%20Artist%20%2b%20Guest%20lyrics") {
		t.Fatalf("missing or incorrectly escaped search link: %s", missing.Body.String())
	}

	if !strings.Contains(missing.Body.String(), "https://www.google.com/search?q=A%20%26%20B%20by%20Artist%20%2b%20Guest%20song%20description") {
		t.Fatal("missing description search link")
	}

	complete := request(h, "GET", "/collections/search/songs/complete", nil)
	if strings.Contains(complete.Body.String(), "https://www.google.com/search?") {
		t.Fatal("search link displayed for a song with lyrics")
	}
}

func TestSaveAndNextMissingEvidence(t *testing.T) {
	h, svc := handler(t)
	c := domain.Collection{ID: "next", Songs: []domain.Song{
		{Metadata: domain.Metadata{ID: "first", Title: "First"}},
		{Metadata: domain.Metadata{ID: "ready", Title: "Ready"}, Lyrics: "already has lyrics"},
		{Metadata: domain.Metadata{ID: "last", Title: "Last"}},
	}}
	if err := svc.Store.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}

	page := request(h, "GET", "/collections/next/songs/first", nil).Body.String()
	if !strings.Contains(page, "2 songs still need lyrics or a description, including this song.") {
		t.Fatal("missing evidence count not shown on song page")
	}
	start := strings.Index(page, `name="csrf" value="`) + len(`name="csrf" value="`)
	token := strings.Split(page[start:], `"`)[0]
	response := request(h, "POST", "/collections/next/songs/first", url.Values{"csrf": {token}, "description": {"My description"}, "next": {"1"}})
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/collections/next/songs/last" {
		t.Fatalf("unexpected next song: %d %s", response.Code, response.Header().Get("Location"))
	}

	saved, err := svc.Store.Get(context.Background(), "next")
	if err != nil || saved.Song("first").Description != "My description" {
		t.Fatalf("description not saved: %v", err)
	}

	response = request(h, "POST", "/collections/next/songs/last", url.Values{"csrf": {token}, "lyrics": {"Words"}, "next": {"1"}})
	if response.Code != http.StatusSeeOther || !strings.Contains(response.Header().Get("Location"), "No+other+songs+need") {
		t.Fatalf("unexpected end of workflow: %d %s", response.Code, response.Header().Get("Location"))
	}
}

type authorizingAccount struct{ account }

func (authorizingAccount) Authorization(bool) (string, string, error) {
	return "https://login.tidal.com/authorize", "test-state", nil
}

func TestExportAuthorizationReturnsToCollection(t *testing.T) {
	_, svc := handler(t)
	if err := svc.Store.Save(context.Background(), domain.Collection{ID: "mycollection"}); err != nil {
		t.Fatal(err)
	}
	h, err := New(svc, authorizingAccount{}, "localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	home := request(h, "GET", "/", nil).Body.String()
	start := strings.Index(home, `name="csrf" value="`) + len(`name="csrf" value="`)
	token := strings.Split(home[start:], `"`)[0]
	response := request(h, "POST", "/connect", url.Values{"csrf": {token}, "write": {"1"}, "collection": {"mycollection"}})
	if response.Header().Get("Location") != "https://login.tidal.com/authorize" {
		t.Fatal("authorization did not begin")
	}

	r := httptest.NewRequest("GET", "http://localhost:8080/tidal/callback?state=test-state&code=test", nil)
	for _, cookie := range response.Result().Cookies() {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/collections/mycollection/export" {
		t.Fatalf("unexpected callback redirect: %d %s", w.Code, w.Header().Get("Location"))
	}
}

func TestPreviewErrorReturnsToExportSection(t *testing.T) {
	h, svc := handler(t)
	if err := svc.Store.Save(context.Background(), domain.Collection{ID: "preview"}); err != nil {
		t.Fatal(err)
	}
	page := request(h, "GET", "/", nil).Body.String()
	start := strings.Index(page, `name="csrf" value="`) + len(`name="csrf" value="`)
	token := strings.Split(page[start:], `"`)[0]
	response := request(h, "POST", "/collections/preview/preview", url.Values{"csrf": {token}})
	u, err := url.Parse(response.Header().Get("Location"))
	if err != nil || u.Path != "/collections/preview/export" || u.Query().Get("message") == "" {
		t.Fatalf("unexpected preview redirect: %s", response.Header().Get("Location"))
	}
}

func TestExportPageDisplaysErrorsAndOmittedTracks(t *testing.T) {
	h, svc := handler(t)
	c := domain.Collection{
		ID: "export-page", Name: "Collection", SourceID: "source",
		Job:     domain.Job{Kind: "export", State: "paused", Message: "A provider error"},
		Exports: []domain.Export{{Confirmed: true, State: "paused", Error: "Export failed", Omitted: []domain.Metadata{{ID: "123", Title: "Missing Song", Artist: "Artist"}}}},
	}
	if err := svc.Store.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}

	response := request(h, "GET", "/collections/export-page/export", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Export failed") || !strings.Contains(response.Body.String(), "Missing Song") {
		t.Fatalf("export details not displayed: %d %s", response.Code, response.Body.String())
	}
	collection := request(h, "GET", "/collections/export-page", nil)
	if !strings.Contains(collection.Body.String(), `/collections/export-page/export`) {
		t.Fatal("collection does not link to export page")
	}
}

func TestCreatePlaylistsButtonAlwaysShowsRemainingAssignments(t *testing.T) {
	h, svc := handler(t)
	c := domain.Collection{ID: "export-ready", SourceID: "source", Songs: []domain.Song{{Metadata: domain.Metadata{ID: "song"}}}}
	if err := svc.Store.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}

	page := request(h, "GET", "/collections/export-ready", nil).Body.String()
	if !strings.Contains(page, `data-create-playlists aria-describedby="evidence-status" disabled`) || !strings.Contains(page, `data-count="Unassigned">1</span>`) {
		t.Fatal("playlist creation button or remaining assignment count missing")
	}

	if err := svc.Store.Update(context.Background(), "export-ready", func(c *domain.Collection) error {
		c.Songs[0].Description = "Evidence"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ready := request(h, "GET", "/collections/export-ready", nil).Body.String()
	if !strings.Contains(ready, `data-create-playlists aria-describedby="evidence-status" >`) {
		t.Fatal("playlist button is not enabled when all songs have evidence")
	}

	export := request(h, "GET", "/collections/export-ready/export", nil).Body.String()
	if strings.Contains(export, "Songs awaiting assignment") || !strings.Contains(export, "Generate playlist preview") {
		t.Fatal("export steps not shown")
	}
}

func TestExportPageSeparatesPreviewAndPreviousExports(t *testing.T) {
	h, svc := handler(t)
	c := domain.Collection{ID: "export-review", Name: "Collection", SourceID: "source", Revision: 2, Exports: []domain.Export{
		{ID: "past", Confirmed: true, State: "completed", Playlists: []domain.ExportPlaylist{{Name: "Old playlist", Destination: "destination", Tracks: []domain.Metadata{{Title: "Old song"}}}}},
		{ID: "current", State: "preview", Revision: 2, Playlists: []domain.ExportPlaylist{{Name: "New playlist", Tracks: []domain.Metadata{{Title: "New song"}}}}},
	}}
	if err := svc.Store.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}

	page := request(h, "GET", "/collections/export-review/export", nil).Body.String()
	if !strings.Contains(page, "Current preview") || !strings.Contains(page, "Update preview") || !strings.Contains(page, "Previous exports (1)") || !strings.Contains(page, "New song") || strings.Contains(page, "Old song") {
		t.Fatal("preview and export history are not clearly separated")
	}
}

func TestClassificationRequiresTwoCategories(t *testing.T) {
	h, svc := handler(t)
	c := domain.Collection{ID: "categories", Songs: []domain.Song{{Metadata: domain.Metadata{ID: "track"}, Description: "Evidence"}}}
	if err := svc.Store.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}

	for count := 0; count <= 2; count++ {
		if err := svc.Store.Update(context.Background(), "categories", func(c *domain.Collection) error {
			c.Categories = nil
			for i := 0; i < count; i++ {
				c.Categories = append(c.Categories, domain.Category{ID: fmt.Sprint(i), Name: fmt.Sprint(i)})
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		page := request(h, "GET", "/collections/categories", nil).Body.String()
		message := "Add at least two categories to classify songs."
		if count < 2 && (!strings.Contains(page, `title="`+message+`"`) || !strings.Contains(page, `aria-describedby="category-requirement" disabled`)) {
			t.Fatalf("missing disabled classification explanation with %d categories", count)
		}
		if count == 2 && strings.Contains(page, `id="category-requirement"`) {
			t.Fatal("classification still requires categories after two were added")
		}
	}
}
