package tidal

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	c := New(Config{SessionPath: filepath.Join(t.TempDir(), ".session.json"), Country: "US", ClientID: "test-client", RedirectURL: "http://127.0.0.1:8080/tidal/callback"})
	c.Base = server.URL + "/v2"
	c.session = session{Access: "test-access", Refresh: "test-refresh", Until: time.Now().Add(time.Hour), Scope: "playlists.read playlists.write"}

	return c
}

func TestLargeImportUsesOnlyPlaylistEndpoints(t *testing.T) {
	requests := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++

		if r.Header.Get("Authorization") != "Bearer test-access" {
			t.Error("missing authorization")
		}

		if r.URL.Path == "/v2/playlists/source" {
			fmt.Fprint(w, `{"data":{"type":"playlists","id":"source","attributes":{"name":"Large source"}}}`)

			return
		}

		if r.URL.Path != "/v2/playlists/source/relationships/items" {
			t.Errorf("unexpected metadata lookup: %s", r.URL.Path)
			http.Error(w, "unexpected", 400)

			return
		}

		page, _ := strconv.Atoi(r.URL.Query().Get("page[cursor]"))
		data := []resource{}
		included := []resource{}

		for i := page * 100; i < (page+1)*100; i++ {
			id := fmt.Sprint(i)
			data = append(data, resource{ID: id, Type: "tracks"})
			attrs := map[string]any{"title": "Song " + id, "duration": "PT3M12S"}

			if i == 1299 {
				delete(attrs, "title")
			}

			included = append(included, resource{ID: id, Type: "tracks", Attributes: attrs, Relationships: map[string]relationship{"artists": {Data: json.RawMessage(`[{"id":"artist","type":"artists"}]`)}, "albums": {Data: json.RawMessage(`[{"id":"album","type":"albums"}]`)}}})
		}

		included = append(included, resource{ID: "artist", Type: "artists", Attributes: map[string]any{"name": "Artist"}}, resource{ID: "album", Type: "albums", Attributes: map[string]any{"title": "Album"}})
		links := map[string]any{}

		if page < 12 {
			if page%2 == 0 {
				links["next"] = map[string]string{"href": fmt.Sprintf("/playlists/source/relationships/items?page[cursor]=%d", page+1)}
			} else {
				links["next"] = fmt.Sprintf("?page[cursor]=%d", page+1)
			}
		}

		json.NewEncoder(w).Encode(map[string]any{"data": data, "included": included, "links": links})
	})
	name, tracks, err := c.Import(context.Background(), "source")
	if err != nil {
		t.Fatal(err)
	}

	if name != "Large source" || len(tracks) != 1300 || requests != 14 {
		t.Fatalf("name=%s tracks=%d requests=%d", name, len(tracks), requests)
	}

	for i, m := range tracks {
		if m.ID != fmt.Sprint(i) || m.Artist != "Artist" || m.Album != "Album" || m.Duration != 192 {
			t.Fatalf("bad metadata at %d: %+v", i, m)
		}
	}

	if tracks[1299].Title != "" {
		t.Fatal("missing title was invented")
	}
}

func TestPaginationRejectsForeignOrigin(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[],"links":{"next":"https://example.com/steal"}}`)
	})
	if _, err := c.Playlists(context.Background()); err == nil {
		t.Fatal("foreign pagination accepted")
	}
}

func TestOAuthPKCEPersistenceAndState(t *testing.T) {
	var challenge string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))

		if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			t.Error("PKCE verifier mismatch")
		}

		fmt.Fprint(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600,"scope":"playlists.read"}`)
	})
	c.TokenURL = strings.TrimSuffix(c.Base, "/v2") + "/token"
	u, state, err := c.Authorization(false)
	if err != nil {
		t.Fatal(err)
	}

	parsed, _ := url.Parse(u)
	challenge = parsed.Query().Get("code_challenge")

	if parsed.Query().Get("code_challenge_method") != "S256" || strings.Contains(parsed.Query().Get("scope"), "write") {
		t.Fatal("bad authorization parameters")
	}

	if err = c.Callback(context.Background(), "invalid", "code"); err == nil {
		t.Fatal("invalid state accepted")
	}

	if err = c.Callback(context.Background(), state, "code"); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(c.Config.SessionPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("session permissions", err)
	}

	reloaded := New(c.Config)

	if !reloaded.Connected() {
		t.Fatal("saved session not restored")
	}

	if err = c.Callback(context.Background(), state, "code"); err == nil {
		t.Fatal("state replay accepted")
	}
}

func TestAddHandlesAlreadyPresentAndUnavailable(t *testing.T) {
	reason := "ALREADY_PRESENT"
	var key string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		key = r.Header.Get("Idempotency-Key")
		var payload struct {
			Meta struct {
				OnDuplicates string `json:"onDuplicates"`
			}
		}
		json.NewDecoder(r.Body).Decode(&payload)

		if payload.Meta.OnDuplicates != "SKIP" {
			t.Error("duplicate protection missing")
		}

		fmt.Fprintf(w, `{"data":[{"id":"1","type":"tracks"}],"meta":{"skipped":[{"id":"2","type":"tracks","reason":%q}]}}`, reason)
	})
	if err := c.Add(context.Background(), "dest", []string{"1", "2"}, "stable-key"); err != nil {
		t.Fatal(err)
	}

	if key != "stable-key" {
		t.Fatal("missing idempotency key")
	}

	reason = "NOT_FOUND"
	if err := c.Add(context.Background(), "dest", []string{"1", "2"}, "stable-key"); err == nil {
		t.Fatal("unavailable track reported as success")
	}
}

func TestPaginationRejectsAPITraversal(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { t.Fatal("unsafe path was requested") })

	for _, link := range []string{"/../../secret", "//example.com/v2/playlists", "http://example.com/v2/playlists", "/v2/../secret"} {
		if err := c.request(context.Background(), "GET", link, nil, "", nil); err == nil {
			t.Errorf("accepted unsafe pagination link %q", link)
		}
	}
}

func TestImportMatchesMetadataByRelationshipIDs(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/playlists/source" {
			fmt.Fprint(w, `{"data":{"id":"source","type":"playlists","attributes":{"name":"Test"}}}`)
			return
		}
		if got := r.URL.Query().Get("include"); got != "items.artists,items.albums" {
			t.Errorf("include = %q", got)
		}
		fmt.Fprint(w, `{"data":[{"id":"t1","type":"tracks"},{"id":"t2","type":"tracks"}],"included":[{"id":"a2","type":"artists","attributes":{"name":"Second artist"}},{"id":"b1","type":"albums","attributes":{"title":"First album"}},{"id":"t1","type":"tracks","attributes":{"title":"First song","bpm":120.5,"key":"CSharp","keyScale":"MINOR"},"relationships":{"artists":{"data":[{"id":"a1","type":"artists"}]},"albums":{"data":[{"id":"b1","type":"albums"}]}}},{"id":"b2","type":"albums","attributes":{"title":"Second album"}},{"id":"t2","type":"tracks","attributes":{"title":"Second song"},"relationships":{"artists":{"data":[{"id":"a2","type":"artists"}]},"albums":{"data":[{"id":"b2","type":"albums"}]}}},{"id":"a1","type":"artists","attributes":{"name":"First artist"}}]}`)
	})
	_, songs, err := c.Import(context.Background(), "source")
	if err != nil {
		t.Fatal(err)
	}
	if len(songs) != 2 || songs[0].Artist != "First artist" || songs[0].Album != "First album" || songs[1].Artist != "Second artist" || songs[1].Album != "Second album" {
		t.Fatalf("incorrect relationship matching: %+v", songs)
	}
	if songs[0].BPM == nil || *songs[0].BPM != 120.5 || songs[0].Key != "CSharp" || songs[0].KeyScale != "MINOR" {
		t.Fatalf("audio metadata not imported: %+v", songs[0].Metadata)
	}
	if songs[1].BPM != nil || songs[1].Key != "" || songs[1].KeyScale != "" {
		t.Fatal("missing audio metadata should stay absent")
	}

}

func TestImportUsesOnlyLinkedIncludedLyrics(t *testing.T) {
	calls := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path == "/v2/playlists/source" {
			fmt.Fprint(w, `{"data":{"type":"playlists","id":"source","attributes":{"name":"Source"}}}`)
			return
		}
		if r.URL.Path != "/v2/playlists/source/relationships/items" {
			t.Errorf("unexpected extra request %s", r.URL.Path)
			http.Error(w, "unexpected", 400)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"1","type":"tracks"},{"id":"2","type":"tracks"}],"included":[{"id":"1","type":"tracks","attributes":{"title":"First"},"relationships":{"lyrics":{"data":[{"id":"lyric-1","type":"lyrics"}]}}},{"id":"2","type":"tracks","attributes":{"title":"Second"}},{"id":"lyric-1","type":"lyrics","attributes":{"technicalStatus":"OK","text":"Matched lyrics"}},{"id":"unrelated","type":"lyrics","attributes":{"technicalStatus":"OK","text":"Wrong lyrics"}}]}`)
	})
	_, tracks, err := c.Import(context.Background(), "source")
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 || tracks[0].Lyrics != "Matched lyrics" || tracks[1].Lyrics != "" || calls != 2 {
		t.Fatalf("linked lyrics not handled safely: tracks=%d calls=%d", len(tracks), calls)
	}
}

func TestIncludedLyricsRejectsUnreadyOrConflictingResults(t *testing.T) {
	track := resource{Relationships: map[string]relationship{"lyrics": {Data: json.RawMessage(`[{"id":"1","type":"lyrics"},{"id":"2","type":"lyrics"}]`)}}}
	index := map[string]resource{"lyrics:1": {Attributes: map[string]any{"technicalStatus": "OK", "text": "First"}}, "lyrics:2": {Attributes: map[string]any{"technicalStatus": "OK", "text": "Different"}}}
	if got := includedLyrics(track, index); got != "" {
		t.Fatal("conflicting lyrics were accepted")
	}
	index["lyrics:2"] = resource{Attributes: map[string]any{"technicalStatus": "PROCESSING", "text": "Different"}}
	if got := includedLyrics(track, index); got != "First" {
		t.Fatal("unready lyrics were used")
	}
}
