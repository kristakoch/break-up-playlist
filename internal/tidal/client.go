// Package tidal owns OAuth credentials and the TIDAL JSON:API adapter.
package tidal

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kristakoch/break-up-playlist/internal/app"
	"github.com/kristakoch/break-up-playlist/internal/domain"
)

type Config struct{ ClientID, ClientSecret, RedirectURL, SessionPath, Country string }
type session struct {
	Access  string    `json:"access_token"`
	Refresh string    `json:"refresh_token"`
	Scope   string    `json:"scope"`
	Expires int       `json:"expires_in"`
	Until   time.Time `json:"expires_at"`
}
type flow struct {
	verifier string
	expires  time.Time
}
type Client struct {
	Config                       Config
	Base, TokenURL, AuthorizeURL string
	HTTP                         *http.Client
	mu                           sync.Mutex
	session                      session
	flows                        map[string]flow
	loadErr                      error
}

func New(cfg Config) *Client {
	c := &Client{Config: cfg, Base: "https://openapi.tidal.com/v2", TokenURL: "https://auth.tidal.com/v1/oauth2/token", AuthorizeURL: "https://login.tidal.com/authorize", HTTP: &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, flows: map[string]flow{}}
	b, err := os.ReadFile(cfg.SessionPath)
	if err == nil {
		info, e := os.Stat(cfg.SessionPath)

		if e != nil || info.Mode().Perm()&0077 != 0 {
			c.loadErr = errors.New("TIDAL session file must be owner-only (chmod 600)")
		} else {
			c.loadErr = json.Unmarshal(b, &c.session)
		}
	} else if !os.IsNotExist(err) {
		c.loadErr = err
	}

	return c
}

func random() string {
	b := make([]byte, 32)

	if _, e := rand.Read(b); e != nil {
		panic(e)
	}

	return base64.RawURLEncoding.EncodeToString(b)
}

func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.session.Access != "" && c.loadErr == nil
}

func (c *Client) CanWrite() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return strings.Contains(" "+c.session.Scope+" ", " playlists.write ")
}

func (c *Client) Authorization(write bool) (string, string, error) {
	if c.Config.ClientID == "" {
		return "", "", errors.New("set TIDAL_CLIENT_ID and restart to connect TIDAL")
	}

	state, verifier := random(), random()
	h := sha256.Sum256([]byte(verifier))
	scope := "playlists.read"

	if write {
		scope += " playlists.write"
	}

	c.mu.Lock()

	for k, v := range c.flows {
		if time.Now().After(v.expires) {
			delete(c.flows, k)
		}
	}

	c.flows[state] = flow{verifier, time.Now().Add(10 * time.Minute)}
	c.mu.Unlock()
	q := url.Values{"response_type": {"code"}, "client_id": {c.Config.ClientID}, "redirect_uri": {c.Config.RedirectURL}, "scope": {scope}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(h[:])}, "state": {state}}

	return c.AuthorizeURL + "?" + q.Encode(), state, nil
}

func (c *Client) Callback(ctx context.Context, state, code string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.flows[state]
	delete(c.flows, state)

	if !ok || time.Now().After(f.expires) || code == "" {
		return errors.New("authorization expired or state is invalid; reconnect")
	}

	return c.exchange(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {c.Config.RedirectURL}, "code_verifier": {f.verifier}})
}

// exchange is called with mu held; credentials never leave this package.
func (c *Client) exchange(ctx context.Context, q url.Values) error {
	q.Set("client_id", c.Config.ClientID)

	if c.Config.ClientSecret != "" {
		q.Set("client_secret", c.Config.ClientSecret)
	}

	req, _ := http.NewRequestWithContext(ctx, "POST", c.TokenURL, strings.NewReader(q.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r, err := c.HTTP.Do(req)
	if err != nil {
		return errors.New("TIDAL authorization service is unavailable; reconnect later")
	}

	defer r.Body.Close()

	if r.StatusCode != 200 {
		return fmt.Errorf("TIDAL authorization failed (HTTP %d); reconnect and check developer app permissions", r.StatusCode)
	}

	var next session

	if err = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&next); err != nil {
		return errors.New("invalid TIDAL authorization response")
	}

	if next.Access == "" {
		return errors.New("TIDAL did not return an access token")
	}

	if next.Refresh == "" {
		next.Refresh = c.session.Refresh
	}

	if next.Scope == "" {
		next.Scope = c.session.Scope
	}

	next.Until = time.Now().Add(time.Duration(next.Expires) * time.Second)
	if err = os.MkdirAll(filepath.Dir(c.Config.SessionPath), 0700); err != nil {
		return err
	}

	f, err := os.CreateTemp(filepath.Dir(c.Config.SessionPath), ".tidal-session-*")
	if err != nil {
		return err
	}

	defer os.Remove(f.Name())

	if err = json.NewEncoder(f).Encode(next); err != nil {
		f.Close()

		return err
	}

	if err = f.Sync(); err != nil {
		f.Close()

		return err
	}

	if err = f.Close(); err != nil {
		return err
	}

	if err = os.Rename(f.Name(), c.Config.SessionPath); err != nil {
		return err
	}

	c.session = next
	c.loadErr = nil

	return nil
}

func (c *Client) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.loadErr != nil {
		return "", c.loadErr
	}

	if c.session.Access == "" {
		return "", errors.New("connect TIDAL first")
	}

	if time.Until(c.session.Until) < time.Minute {
		if c.session.Refresh == "" {
			return "", errors.New("TIDAL session expired; reconnect")
		}

		if err := c.exchange(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c.session.Refresh}}); err != nil {
			return "", err
		}
	}

	return c.session.Access, nil
}

type requestError struct {
	status int
	delay  time.Duration
}

func (e requestError) Error() string {
	if e.status == 401 || e.status == 403 {
		return "TIDAL access denied; reconnect with the required playlist permissions"
	}

	return fmt.Sprintf("TIDAL request failed (HTTP %d); completed work is saved", e.status)
}

func (e requestError) Temporary() bool {
	return e.status == 429 || e.status == 502 || e.status == 503 || e.status == 504
}

func (e requestError) RetryDelay() time.Duration { return e.delay }

func (c *Client) request(ctx context.Context, method, path string, payload any, key string, out any) error {
	base, _ := url.Parse(c.Base + "/")
	u, err := url.Parse(path)
	if err != nil {
		return errors.New("invalid TIDAL page URL")
	}

	if !u.IsAbs() {
		// TIDAL returns root-relative links such as /playlists/... even though
		// requests use the /v2 API prefix. Preserve that prefix for those links.
		if u.Host == "" && strings.HasPrefix(u.Path, "/") && !strings.HasPrefix(u.Path, strings.TrimSuffix(base.Path, "/")+"/") {
			u.Path = strings.TrimSuffix(base.Path, "/") + u.Path
		}

		u = base.ResolveReference(u)
	}

	if u.User != nil || u.Scheme != base.Scheme || u.Host != base.Host || !strings.HasPrefix(pathpkg.Clean(u.Path), strings.TrimSuffix(base.Path, "/")+"/") {
		return errors.New("TIDAL returned a page outside its API origin")
	}

	var data []byte

	if payload != nil {
		data, err = json.Marshal(payload)
		if err != nil {
			return err
		}
	}

	for attempt := 0; attempt < 4; attempt++ {
		token, err := c.token(ctx)
		if err != nil {
			return err
		}

		req, _ := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.api+json")

		if payload != nil {
			req.Header.Set("Content-Type", "application/vnd.api+json")
		}

		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}

		r, err := c.HTTP.Do(req)
		if err != nil {
			return errors.New("TIDAL network request failed; retry to resume saved work")
		}

		slog.Info("TIDAL operation", "method", method, "path", u.Path, "status", r.StatusCode)

		if r.StatusCode >= 200 && r.StatusCode < 300 {
			defer r.Body.Close()

			if out != nil && r.StatusCode != 204 {
				if err = json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(out); err != nil {
					return errors.New("invalid TIDAL response; import or export is incomplete")
				}
			}

			return nil
		}

		delay := time.Second * time.Duration(1<<attempt)

		if secs, e := strconv.Atoi(r.Header.Get("Retry-After")); e == nil {
			delay = max(delay, time.Duration(secs)*time.Second)
		} else if t, e := http.ParseTime(r.Header.Get("Retry-After")); e == nil {
			delay = max(delay, time.Until(t))
		}

		status := r.StatusCode
		r.Body.Close()
		e := requestError{status, delay}

		if method != "GET" || !e.Temporary() || attempt == 3 {
			return e
		}

		t := time.NewTimer(delay)

		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}

	return errors.New("TIDAL retries exhausted")
}

type resource struct {
	ID, Type      string
	Attributes    map[string]any
	Relationships map[string]relationship
	Meta          map[string]any
}
type relationship struct {
	Data  json.RawMessage
	Links map[string]json.RawMessage
}
type document struct {
	Data     json.RawMessage
	Included []resource
	Links    map[string]json.RawMessage
	Meta     map[string]any
}

func nextLink(links map[string]json.RawMessage) string {
	b := links["next"]
	var s string

	if json.Unmarshal(b, &s) == nil {
		return s
	}

	var o struct{ Href string }
	_ = json.Unmarshal(b, &o)

	return o.Href
}

// Resolve query-only and relative links against the current page, not the API root.
func nextPage(current, next string) string {
	if next == "" {
		return ""
	}

	base, err := url.Parse(current)
	if err != nil {
		return next
	}

	ref, err := url.Parse(next)
	if err != nil {
		return next
	}

	return base.ResolveReference(ref).String()
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func resources(raw json.RawMessage) []resource {
	var a []resource

	if json.Unmarshal(raw, &a) == nil {
		return a
	}

	var r resource

	if json.Unmarshal(raw, &r) == nil && r.ID != "" {
		return []resource{r}
	}

	return nil
}

func (c *Client) Playlists(ctx context.Context) ([]app.Playlist, error) {
	path := "playlists?" + url.Values{"filter[owners.id]": {"me"}, "countryCode": {c.Config.Country}}.Encode()
	var out []app.Playlist
	seen := map[string]bool{}

	for path != "" {
		if seen[path] {
			return nil, errors.New("TIDAL pagination loop")
		}

		seen[path] = true
		var d document

		if err := c.request(ctx, "GET", path, nil, "", &d); err != nil {
			return nil, err
		}

		for _, r := range resources(d.Data) {
			out = append(out, app.Playlist{ID: r.ID, Name: str(r.Attributes, "name")})
		}

		path = nextPage(path, nextLink(d.Links))
	}

	return out, nil
}

func (c *Client) Import(ctx context.Context, id string) (string, []app.ImportedTrack, error) {
	if strings.TrimSpace(id) == "" {
		return "", nil, errors.New("playlist ID is required")
	}

	var head document

	if err := c.request(ctx, "GET", "playlists/"+url.PathEscape(id)+"?countryCode="+url.QueryEscape(c.Config.Country), nil, "", &head); err != nil {
		return "", nil, err
	}

	h := resources(head.Data)

	if len(h) != 1 {
		return "", nil, errors.New("TIDAL returned no playlist")
	}

	name := str(h[0].Attributes, "name")

	if name == "" {
		name = "Playlist " + id
	}

	path := "playlists/" + url.PathEscape(id) + "/relationships/items?" + url.Values{"countryCode": {c.Config.Country}, "include": {"items.artists,items.albums"}, "sort": {"itemIndex"}}.Encode()
	seen := map[string]bool{}
	var out []app.ImportedTrack

	for path != "" {
		if seen[path] {
			return "", nil, errors.New("TIDAL pagination loop; import incomplete")
		}

		seen[path] = true
		var d document

		if err := c.request(ctx, "GET", path, nil, "", &d); err != nil {
			return "", nil, err
		}

		index := map[string]resource{}

		for _, r := range d.Included {
			index[r.Type+":"+r.ID] = r
		}

		for _, link := range resources(d.Data) {
			if link.Type != "tracks" {
				continue
			}

			r := link

			if full, ok := index[link.Type+":"+link.ID]; ok {
				r = full
			}

			m := domain.Metadata{ID: r.ID, Title: str(r.Attributes, "title"), Catalog: map[string]any{"track": r.Attributes}}
			if bpm, ok := r.Attributes["bpm"].(float64); ok && bpm > 0 {
				m.BPM = &bpm
			}

			if key := str(r.Attributes, "key"); key != "" && key != "UNKNOWN" {
				m.Key = key
			}
			if scale := str(r.Attributes, "keyScale"); scale != "" && scale != "UNKNOWN" {
				m.KeyScale = scale
			}

			duration := str(r.Attributes, "duration")
			m.Duration = parseDuration(duration)

			if n, ok := r.Attributes["duration"].(float64); ok {
				m.Duration = n
			}

			for rel, field := range map[string]*string{"artists": &m.Artist, "albums": &m.Album} {
				values := []string{}
				catalog := []map[string]any{}

				for _, v := range resources(r.Relationships[rel].Data) {
					if full, ok := index[v.Type+":"+v.ID]; ok {
						v = full
					}

					label := str(v.Attributes, "name")

					if rel == "albums" {
						label = str(v.Attributes, "title")
					}

					if label != "" {
						values = append(values, label)
					}

					if v.Attributes != nil {
						catalog = append(catalog, v.Attributes)
					}
				}

				*field = strings.Join(values, ", ")

				if len(catalog) > 0 {
					m.Catalog[rel] = catalog
				}
			}

			out = append(out, app.ImportedTrack{Metadata: m, Lyrics: includedLyrics(r, index)})
		}

		slog.Info("TIDAL import progress", "tracks", len(out))
		path = nextPage(path, nextLink(d.Links))
	}

	return name, out, nil
}

// Read only lyric resources already included and linked to this exact track.
// TIDAL's lyric endpoints are internal-only, so this never performs extra calls.
func includedLyrics(track resource, index map[string]resource) string {
	var lyrics string
	for _, link := range resources(track.Relationships["lyrics"].Data) {
		if link.Type != "lyrics" {
			continue
		}
		item, ok := index[link.Type+":"+link.ID]
		if !ok || str(item.Attributes, "technicalStatus") != "OK" {
			continue
		}
		text := str(item.Attributes, "text")
		if strings.TrimSpace(text) == "" {
			text = str(item.Attributes, "lrcText")
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		if lyrics != "" && lyrics != text {
			return ""
		}
		lyrics = text
	}
	return lyrics
}

func parseDuration(s string) float64 {
	if !strings.HasPrefix(s, "PT") {
		return 0
	}

	s = strings.TrimPrefix(s, "PT")
	n, total := "", 0.0

	for _, r := range s {
		if r >= '0' && r <= '9' || r == '.' {
			n += string(r)
			continue
		}

		v, _ := strconv.ParseFloat(n, 64)

		switch r {
		case 'H':
			total += v * 3600
		case 'M':
			total += v * 60
		case 'S':
			total += v
		}

		n = ""
	}

	return total
}

func (c *Client) Create(ctx context.Context, name, key string) (string, error) {
	if !c.CanWrite() {
		return "", errors.New("authorize TIDAL playlist creation first")
	}

	payload := map[string]any{"data": map[string]any{"type": "playlists", "attributes": map[string]any{"name": name, "accessType": "UNLISTED"}}}
	var d document

	if err := c.request(ctx, "POST", "playlists", payload, key, &d); err != nil {
		return "", err
	}

	rs := resources(d.Data)

	if len(rs) != 1 || rs[0].Type != "playlists" || rs[0].ID == "" {
		return "", errors.New("TIDAL returned an invalid created playlist")
	}

	if str(rs[0].Attributes, "accessType") != "UNLISTED" {
		return "", errors.New("TIDAL did not confirm unlisted visibility; no tracks added")
	}

	return rs[0].ID, nil
}

func (c *Client) Add(ctx context.Context, destination string, ids []string, key string) error {
	if !c.CanWrite() {
		return errors.New("authorize TIDAL playlist creation first")
	}

	data := []map[string]string{}

	for _, id := range ids {
		data = append(data, map[string]string{"id": id, "type": "tracks"})
	}

	payload := map[string]any{"data": data, "meta": map[string]string{"onDuplicates": "SKIP"}}
	var d document

	if err := c.request(ctx, "POST", "playlists/"+url.PathEscape(destination)+"/relationships/items", payload, key, &d); err != nil {
		return err
	}

	present := map[string]bool{}

	for _, r := range resources(d.Data) {
		if r.Type == "tracks" {
			present[r.ID] = true
		}
	}

	requested := make(map[string]bool, len(ids))
	for _, id := range ids {
		requested[id] = true
	}

	var unavailable []string
	if skipped, ok := d.Meta["skipped"].([]any); ok {
		for _, v := range skipped {
			entry, ok := v.(map[string]any)
			id := str(entry, "id")
			if !ok || str(entry, "type") != "tracks" || !requested[id] {
				return errors.New("TIDAL returned an unexpected skipped item; export is incomplete")
			}

			switch str(entry, "reason") {
			case "ALREADY_PRESENT":
				present[id] = true
			case "NOT_FOUND":
				unavailable = append(unavailable, id)
			default:
				return errors.New("TIDAL returned an unknown skip reason; export is incomplete")
			}
		}
	}

	if len(unavailable) > 0 {
		return &domain.UnavailableTracksError{IDs: unavailable}
	}

	for _, id := range ids {
		if !present[id] {
			return errors.New("TIDAL did not confirm every added track; inspect and resume export")
		}
	}

	return nil
}
