// Package web serves HTML pages and ordinary forms; JavaScript only refreshes job status.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/kristakoch/break-up-playlist/internal/app"
	"github.com/kristakoch/break-up-playlist/internal/domain"
)

//go:embed *.html *.css *.js
var assets embed.FS

type Account interface {
	Connected() bool
	CanWrite() bool
	Authorization(bool) (string, string, error)
	Callback(context.Context, string, string) error
}
type Server struct {
	App        *app.Service
	Account    Account
	templates  *template.Template
	csrf, host string
}
type group struct {
	Name, Anchor, NextURL, ResetURL string
	CategoryID                      string
	LiveCount                       bool
	Total                           int
	Songs                           []domain.Song
}

type collectionCounts struct {
	Total, Assigned, Unassigned, Ready, Needs, Lyrics, Predictions, MissingEvidence, MissingCatalog int
	Categories                                                                                      map[string]int
}

func countSongs(c domain.Collection) collectionCounts {
	counts := collectionCounts{Total: len(c.Songs), Categories: make(map[string]int)}
	for _, song := range c.Songs {
		if strings.TrimSpace(song.Lyrics) != "" {
			counts.Lyrics++
		}
		if song.Prediction != nil {
			counts.Predictions++
		}
		if assigned := song.Assigned(); assigned != "" {
			counts.Assigned++
			counts.Categories[assigned]++
		}
		if song.Assigned() == "" && song.Eligible() {
			counts.Ready++
		}
		if !song.Eligible() || song.Incomplete() {
			counts.Needs++
		}
		if !song.Eligible() {
			counts.MissingEvidence++
		}
		if song.Incomplete() {
			counts.MissingCatalog++
		}
	}
	counts.Unassigned = counts.Total - counts.Assigned

	return counts
}

type page struct {
	CSRF, Message, Query, Sort, Filter, View    string
	Connected, CanWrite, Blind                  bool
	Collections                                 []domain.Collection
	Collection                                  domain.Collection
	Song                                        *domain.Song
	Groups                                      []group
	CategoryCounts                              map[string]int
	Playlists                                   []app.Playlist
	Job                                         domain.Job
	Lyrics, Predictions, Assigned, Ready, Needs int
	MissingEvidence, MissingCatalog, Unassigned int
	Exports                                     []domain.Export
	Preview, ActiveExport                       *domain.Export
	PreviousExports                             []domain.Export
}

func New(a *app.Service, account Account, host string) (http.Handler, error) {
	funcs := template.FuncMap{"pct": func(v float64) string {
		if v < 0 {
			return "—"
		}

		return fmt.Sprintf("%.1f%%", v*100)
	}, "json": func(v any) string { b, _ := json.MarshalIndent(v, "", "  "); return string(b) }, "remaining": func(j domain.Job) int { return max(0, j.Total-j.Processed) }, "name": func(c domain.Collection, id string) string { return c.CategoryName(id) }, "warning": func(s domain.Song) bool { return s.Prediction != nil && s.Score() < 0.3 }, "active": func(j domain.Job) bool { return j.State == "running" || j.State == "waiting" }, "eqs": func(a, b string) bool { return a == b }}

	t, err := template.New("pages").Funcs(funcs).ParseFS(assets, "*.html")
	if err != nil {
		return nil, err
	}

	s := &Server{App: a, Account: account, templates: t, csrf: app.ID(), host: host}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /collections/{cid}", s.collection)
	mux.HandleFunc("GET /collections/{cid}/songs/{sid}", s.song)
	mux.HandleFunc("GET /collections/{cid}/export", s.exportPage)
	mux.HandleFunc("GET /collections/{cid}/status", s.status)
	mux.HandleFunc("GET /collections/{cid}/report", s.report)
	mux.HandleFunc("GET /tidal/callback", s.callback)
	mux.HandleFunc("POST /import", s.importPlaylist)
	mux.HandleFunc("POST /connect", s.connect)
	mux.HandleFunc("POST /collections/{cid}/{action}", s.action)
	mux.HandleFunc("POST /collections/{cid}/songs/{sid}", s.saveSong)
	mux.HandleFunc("GET /style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		b, _ := assets.ReadFile("style.css")
		w.Write(b)
	})
	mux.HandleFunc("GET /progress.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		b, _ := assets.ReadFile("progress.js")
		w.Write(b)
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Browsers also apply form-action to the authorization redirect after POST /connect.
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self' https://login.tidal.com")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Cache-Control", "no-store")

		if r.Host != s.host {
			slog.Warn("request rejected", "reason", "invalid host", "method", r.Method, "path", r.URL.Path, "host", r.Host)
			http.Error(w, "Invalid host", http.StatusForbidden)

			return
		}

		if r.Method == "POST" {
			origin := r.Header.Get("Origin")

			if origin != "" && origin != "http://"+s.host {
				slog.Warn("request rejected", "reason", "invalid origin", "method", r.Method, "path", r.URL.Path, "origin", origin)
				http.Error(w, "Invalid origin", 403)

				return
			}

			r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
			if err := r.ParseForm(); err != nil {
				slog.Warn("request rejected", "reason", "invalid form", "method", r.Method, "path", r.URL.Path, "error", err)
				http.Error(w, "Invalid form", 400)

				return
			}

			if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(s.csrf)) != 1 {
				slog.Warn("request rejected", "reason", "invalid CSRF token", "method", r.Method, "path", r.URL.Path)
				http.Error(w, "This form expired; reload the page and try again.", 403)

				return
			}
		}

		mux.ServeHTTP(w, r)
	}), nil
}

func (s *Server) base(r *http.Request) page {
	return page{CSRF: s.csrf, Connected: s.Account.Connected(), CanWrite: s.Account.CanWrite(), Message: r.URL.Query().Get("message")}
}

func (s *Server) render(w http.ResponseWriter, name string, p page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if err := s.templates.ExecuteTemplate(w, name, p); err != nil {
		slog.Error("render page", "error", err)
	}
}

func redirect(w http.ResponseWriter, r *http.Request, path string, err error) {
	if err != nil {
		slog.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		parsed, parseErr := url.Parse(path)
		if parseErr == nil {
			query := parsed.Query()
			query.Set("message", err.Error())
			parsed.RawQuery = query.Encode()
			path = parsed.String()
		}
	}

	http.Redirect(w, r, path, http.StatusSeeOther)
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	p := s.base(r)
	var err error
	p.Collections, err = s.App.Store.List(r.Context())
	if err != nil {
		http.Error(w, "Unable to read collections", 500)

		return
	}

	for _, c := range p.Collections {
		s.App.Leave(c.ID)
	}

	if p.Connected && r.URL.Query().Get("browse") == "1" {
		p.Playlists, err = s.App.Music.Playlists(r.Context())
		if err != nil {
			p.Message = err.Error()
		}
	}

	s.render(w, "home", p)
}

func (s *Server) collection(w http.ResponseWriter, r *http.Request) {
	p := s.base(r)
	var err error
	p.Collection, err = s.App.Store.Get(r.Context(), r.PathValue("cid"))
	if err != nil {
		http.NotFound(w, r)

		return
	}

	s.App.AutoEvidence(p.Collection.ID)
	p.Job, _ = s.App.Status(r.Context(), p.Collection.ID)
	p.Query = r.URL.Query().Get("q")
	p.Sort = r.URL.Query().Get("sort")
	p.Filter = r.URL.Query().Get("category")
	p.View = r.URL.Query().Get("view")
	p.Blind = p.View == "blind"
	p.Exports = p.Collection.Exports
	counts := countSongs(p.Collection)
	p.CategoryCounts = counts.Categories
	p.Lyrics, p.Predictions, p.Assigned = counts.Lyrics, counts.Predictions, counts.Assigned
	p.Ready, p.Needs, p.Unassigned = counts.Ready, counts.Needs, counts.Unassigned
	p.MissingEvidence, p.MissingCatalog = counts.MissingEvidence, counts.MissingCatalog

	ids := []string{""}

	if !p.Blind {
		for _, cat := range p.Collection.Categories {
			ids = append(ids, cat.ID)
		}
		ids = append(ids, "missing-evidence")
	}

	for _, id := range ids {
		g := group{Name: p.Collection.CategoryName(id)}
		if id == "missing-evidence" {
			g.Name = "Needs lyrics or a description"
		}

		if p.Blind {
			g.Name = "Label songs independently — predictions hidden"
		}

		for _, song := range p.Collection.Songs {
			songGroup := song.Assigned()
			if songGroup == "" && !song.Eligible() {
				songGroup = "missing-evidence"
			}
			if !p.Blind && songGroup != id {
				continue
			}

			if p.Filter != "" && song.Assigned() != p.Filter {
				continue
			}

			if p.Query != "" && !strings.Contains(strings.ToLower(song.DisplayTitle()+" "+song.Artist), strings.ToLower(p.Query)) {
				continue
			}

			if p.View == "ready" && (song.Assigned() != "" || !song.Eligible()) {
				continue
			}

			if p.View == "needs" && song.Eligible() && !song.Incomplete() {
				continue
			}

			g.Songs = append(g.Songs, song)
		}

		sort.SliceStable(g.Songs, func(i, j int) bool {
			switch p.Sort {
			case "title":
				return strings.ToLower(g.Songs[i].DisplayTitle()) < strings.ToLower(g.Songs[j].DisplayTitle())
			case "score":
				return g.Songs[i].Score() < g.Songs[j].Score()
			default:
				return g.Songs[i].Position < g.Songs[j].Position
			}
		})

		if len(g.Songs) > 0 {
			g.CategoryID = id
			g.LiveCount = !p.Blind && p.Query == "" && p.View == "" && p.Filter == "" && id != "missing-evidence"
			groupID := id
			if groupID == "" {
				groupID = "unassigned"
			}
			if p.Blind {
				groupID = "blind"
			}
			g.Anchor = "group-" + groupID
			g.Total = len(g.Songs)
			param := "show_" + groupID
			limit, err := strconv.Atoi(r.URL.Query().Get(param))
			if err != nil || limit < 10 {
				limit = 10
			}
			if limit < g.Total {
				values := r.URL.Query()
				values.Set(param, strconv.Itoa(min(limit+10, g.Total)))
				g.NextURL = "?" + values.Encode() + "#" + g.Anchor
				g.Songs = g.Songs[:limit]
			}
			if limit > 10 {
				values := r.URL.Query()
				values.Del(param)
				g.ResetURL = "?" + values.Encode() + "#" + g.Anchor
			}
			p.Groups = append(p.Groups, g)
		}
	}

	s.render(w, "collection", p)
}

func (s *Server) exportPage(w http.ResponseWriter, r *http.Request) {
	p := s.base(r)
	var err error
	p.Collection, err = s.App.Store.Get(r.Context(), r.PathValue("cid"))
	if err != nil || p.Collection.SourceID == "" {
		http.NotFound(w, r)
		return
	}

	for i := range p.Collection.Exports {
		export := &p.Collection.Exports[i]
		switch {
		case !export.Confirmed:
			p.Preview = export
		case export.State != "completed":
			p.ActiveExport = export
		default:
			p.PreviousExports = append(p.PreviousExports, *export)
		}
	}

	p.Job, _ = s.App.Status(r.Context(), p.Collection.ID)
	s.render(w, "export", p)
}

func (s *Server) song(w http.ResponseWriter, r *http.Request) {
	p := s.base(r)
	var err error
	p.Collection, err = s.App.Store.Get(r.Context(), r.PathValue("cid"))
	if err != nil {
		http.NotFound(w, r)

		return
	}

	p.Song = p.Collection.Song(r.PathValue("sid"))

	if p.Song == nil {
		http.NotFound(w, r)

		return
	}

	p.Blind = r.URL.Query().Get("blind") == "1"
	p.MissingEvidence = countSongs(p.Collection).MissingEvidence
	s.render(w, "song", p)
}

func (s *Server) saveSong(w http.ResponseWriter, r *http.Request) {
	cid, sid := r.PathValue("cid"), r.PathValue("sid")
	lyrics, desc := r.PostFormValue("lyrics"), r.PostFormValue("description")
	err := s.App.EditSong(r.Context(), cid, sid, lyrics, desc, r.PostFormValue("manual"), r.PostFormValue("reference"))
	path := "/collections/" + cid + "/songs/" + sid

	blind := r.PostFormValue("blind") == "1"
	if err == nil && r.PostFormValue("next") == "1" {
		var collection domain.Collection
		collection, err = s.App.Store.Get(r.Context(), cid)
		if err == nil {
			start := -1
			for i, song := range collection.Songs {
				if song.ID == sid {
					start = i
					break
				}
			}

			found := false
			if start >= 0 {
				for offset := 1; offset < len(collection.Songs); offset++ {
					song := collection.Songs[(start+offset)%len(collection.Songs)]
					if !song.Eligible() {
						path = "/collections/" + cid + "/songs/" + song.ID
						if blind {
							path += "?blind=1"
						}
						found = true
						break
					}
				}
			}

			if !found {
				path = "/collections/" + cid + "?view=needs&message=" + url.QueryEscape("No other songs need lyrics or a description.")
				if blind {
					path = "/collections/" + cid + "?view=blind&message=" + url.QueryEscape("No other songs need lyrics or a description.")
				}
			}
		}
	} else if blind && err == nil {
		path = "/collections/" + cid + "?view=blind"
	}

	redirect(w, r, path, err)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	j, err := s.App.Status(r.Context(), r.PathValue("cid"))
	if err != nil {
		http.NotFound(w, r)

		return
	}
	c, err := s.App.Store.Get(r.Context(), r.PathValue("cid"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		domain.Job
		Counts collectionCounts
	}{Job: j, Counts: countSongs(c)})
}

func (s *Server) importPlaylist(w http.ResponseWriter, r *http.Request) {
	id, err := s.App.Import(r.Context(), strings.TrimSpace(r.PostFormValue("playlist")))
	if err != nil {
		redirect(w, r, "/", err)

		return
	}

	redirect(w, r, "/collections/"+id, nil)
}

func (s *Server) exportReturn(r *http.Request, id string) string {
	if id == "" || strings.Trim(id, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" {
		return "/"
	}
	if _, err := s.App.Store.Get(r.Context(), id); err != nil {
		return "/"
	}
	return "/collections/" + id + "/export"
}

func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	returnPath := s.exportReturn(r, r.PostFormValue("collection"))
	u, state, err := s.Account.Authorization(r.PostFormValue("write") == "1")
	if err != nil {
		redirect(w, r, returnPath, err)
		return
	}

	http.SetCookie(w, &http.Cookie{Name: "tidal_oauth_state", Value: state, Path: "/tidal/callback", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 600})
	if returnPath != "/" {
		http.SetCookie(w, &http.Cookie{Name: "tidal_oauth_collection", Value: r.PostFormValue("collection"), Path: "/tidal/callback", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 600})
	} else {
		http.SetCookie(w, &http.Cookie{Name: "tidal_oauth_collection", Value: "", Path: "/tidal/callback", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	}
	http.Redirect(w, r, u, http.StatusSeeOther)
}

func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("tidal_oauth_state")
	state := r.URL.Query().Get("state")
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(state)) != 1 {
		http.Error(w, "Invalid authorization state; reconnect.", 400)
		return
	}

	returnPath := "/"
	if collection, err := r.Cookie("tidal_oauth_collection"); err == nil {
		returnPath = s.exportReturn(r, collection.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "tidal_oauth_state", Value: "", Path: "/tidal/callback", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.SetCookie(w, &http.Cookie{Name: "tidal_oauth_collection", Value: "", Path: "/tidal/callback", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})

	if r.URL.Query().Get("error") != "" {
		redirect(w, r, returnPath, fmt.Errorf("TIDAL authorization was not completed"))
		return
	}

	redirect(w, r, returnPath, s.Account.Callback(r.Context(), state, r.URL.Query().Get("code")))
}

func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	var err error

	switch r.PathValue("action") {
	case "assign":
		err = s.App.Assign(r.Context(), cid, r.PostFormValue("song"), r.PostFormValue("manual"))
	case "category":
		err = s.App.Category(r.Context(), cid, r.PostFormValue("id"), r.PostFormValue("name"), r.PostFormValue("description"), r.PostFormValue("remove") == "1")
	case "classify":
		err = s.App.Start(cid, "classification", r.PostFormValue("remaining") == "1", r.PostFormValue("bypass") == "1")
	case "lyrics":
		err = s.App.Start(cid, "lyrics", true, false)
	case "stop":
		s.App.Stop(cid)
	case "reset":
		if r.PostFormValue("confirm") != "yes" {
			err = fmt.Errorf("confirm reset first")
		} else {
			err = s.App.Reset(r.Context(), cid, r.PostFormValue("manual") == "1")
		}
	case "delete":
		if r.PostFormValue("confirm") != "yes" {
			err = fmt.Errorf("confirm local deletion first")
		} else {
			err = s.App.Remove(r.Context(), cid)
			if err == nil {
				redirect(w, r, "/", nil)

				return
			}
		}
	case "preview":
		err = s.App.Preview(r.Context(), cid)
	case "confirm":
		if r.PostFormValue("confirm") != "yes" {
			err = fmt.Errorf("confirm creation after reviewing the snapshot")
		} else {
			err = s.App.Confirm(r.Context(), cid, r.PostFormValue("export"))
			if err == nil {
				err = s.App.Start(cid, "export", false, false)
			}
		}
	case "resume":
		err = s.App.Start(cid, "export", false, false)
	case "remove-export":
		if r.PostFormValue("confirm") != "yes" {
			err = fmt.Errorf("confirm removing the export from history")
		} else {
			err = s.App.RemoveExportHistory(r.Context(), cid, r.PostFormValue("export"))
		}

	case "reimport":
		var c domain.Collection
		c, err = s.App.Store.Get(r.Context(), cid)
		if err == nil {
			_, err = s.App.Import(r.Context(), c.SourceID)
		}

	default:
		http.NotFound(w, r)
		return
	}

	path := "/collections/" + cid
	if r.PathValue("action") == "preview" || r.PathValue("action") == "confirm" || r.PathValue("action") == "resume" || r.PathValue("action") == "remove-export" {
		path += "/export"
	}
	redirect(w, r, path, err)
}

func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	c, err := s.App.Store.Get(r.Context(), r.PathValue("cid"))
	if err != nil {
		http.NotFound(w, r)

		return
	}

	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", `attachment; filename="evaluation.csv"`)
		out := csv.NewWriter(w)
		defer out.Flush()
		out.Write([]string{"track_id", "title", "reference", "prediction", "model_score", "review_required"})

		for _, song := range c.Songs {
			prediction, score, review := "", "", ""

			if song.Prediction != nil {
				prediction = c.CategoryName(song.Prediction.Category)
				score = strconv.FormatFloat(song.Score(), 'f', 6, 64)
				review = strconv.FormatBool(song.Prediction.Review)
			}

			ref := ""

			if song.Reference != "" {
				ref = c.CategoryName(song.Reference)
			}

			out.Write([]string{song.ID, song.DisplayTitle(), ref, prediction, score, review})
		}

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="evaluation.json"`)
	json.NewEncoder(w).Encode(domain.Evaluate(c))
}
