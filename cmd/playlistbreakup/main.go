package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kristakoch/break-up-playlist/internal/app"
	"github.com/kristakoch/break-up-playlist/internal/lrclib"
	"github.com/kristakoch/break-up-playlist/internal/lyricsovh"
	"github.com/kristakoch/break-up-playlist/internal/openjev"
	"github.com/kristakoch/break-up-playlist/internal/sqlite"
	"github.com/kristakoch/break-up-playlist/internal/tidal"
	"github.com/kristakoch/break-up-playlist/internal/web"
)

func main() {
	if err := run(); err != nil {
		slog.Error("application stopped", "error", err)
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return def
}

func loadEnv() error {
	f, err := os.Open(".env")
	if os.IsNotExist(err) {
		return nil
	}

	if err != nil {
		return err
	}

	defer f.Close()

	scan := bufio.NewScanner(f)

	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(strings.TrimPrefix(key, "export "))

		if !ok || strings.ContainsAny(key, " \t") {
			return errors.New("invalid .env entry; use KEY=value")
		}

		value = strings.TrimSpace(value)

		if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' || value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}

		if _, exists := os.LookupEnv(key); !exists {
			if err = os.Setenv(key, value); err != nil {
				return err
			}
		}
	}

	return scan.Err()
}

func run() error {
	setup := flag.Bool("setup", false, "install the OpenJev runtime and exit")
	flag.Parse()

	if err := loadEnv(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := env("APP_ADDR", "127.0.0.1:8080")
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}

	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return errors.New("APP_ADDR must bind to loopback")
	}

	cfg, err := openJevConfig()
	if err != nil {
		return err
	}
	classifier, err := openjev.New(cfg.URL, cfg.Model, cfg.Backend, cfg.Weights, os.Getenv("OPENJEV_API_KEY"))
	if err != nil {
		return err
	}
	if env("OPENJEV_MODE", "managed") == "managed" {
		classifier.CacheDir = cfg.Cache
	}

	var listener net.Listener

	if !*setup {
		listener, err = net.Listen("tcp", addr)
		if err != nil {
			return err
		}

		defer listener.Close()
	}

	if *setup {
		return setupOpenJev(ctx, cfg)
	}
	cleanup, err := ensureOpenJev(ctx, cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	data := env("DATA_DIR", ".data")
	if err = os.MkdirAll(data, 0700); err != nil {
		return err
	}

	if err = os.Chmod(data, 0700); err != nil {
		return err
	}

	store, err := sqlite.Open(filepath.Join(data, "collections.db"))
	if err != nil {
		return err
	}

	defer store.Close()

	music := tidal.New(tidal.Config{
		ClientID:     os.Getenv("TIDAL_CLIENT_ID"),
		ClientSecret: os.Getenv("TIDAL_CLIENT_SECRET"),
		RedirectURL:  env("TIDAL_REDIRECT_URL", "http://"+addr+"/tidal/callback"),
		SessionPath:  filepath.Join(data, ".tidal-session.json"),
		Country:      env("TIDAL_COUNTRY", "US"),
	})
	service := app.New(ctx, store, classifier, lrclib.New(), music)
	service.FallbackLyrics = lyricsovh.New()
	defer func() {
		stop()
		service.Wait()
	}()

	if err = service.Recover(ctx); err != nil {
		return err
	}

	handler, err := web.New(service, music, addr)
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       time.Minute,
	}
	done := make(chan error, 1)

	go func() {
		done <- server.Serve(listener)
	}()

	fmt.Printf("Playlist Breakup: http://%s\nPress Ctrl+C to stop.\n", addr)

	select {
	case err = <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}

	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = server.Shutdown(shutdown)
	stop()
	service.Wait()

	return err
}
