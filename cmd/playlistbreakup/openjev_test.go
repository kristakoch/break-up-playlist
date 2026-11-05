package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExistingOpenJevIsReused(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"status":"ok"}`) }))
	defer server.Close()
	t.Setenv("OPENJEV_MODE", "external")
	cleanup, err := ensureOpenJev(context.Background(), modelConfig{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}

	cleanup()
	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal("cleanup stopped external server", err)
	}
	resp.Body.Close()
}

func TestOccupiedOpenJevPortIsNotReused(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "not OpenJev", 503) }))
	defer server.Close()
	t.Setenv("OPENJEV_MODE", "managed")
	_, err := ensureOpenJev(context.Background(), modelConfig{URL: server.URL, Python: "unused"})
	if err == nil || !strings.Contains(err.Error(), "port is occupied") {
		t.Fatalf("expected occupied-port error, got %v", err)
	}
}
