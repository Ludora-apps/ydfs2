package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompatRoutes(t *testing.T) {
	m := newCompatMux()
	m.HandleFunc("GET /api/profiles/{name}", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(pathValue(r, "name"))) })
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/api/profiles/a%20b", "a b", 200},
		{"GET", "/api/profiles/a%2Fb", "a/b", 200},
		{"HEAD", "/api/profiles/a", "a", 200},
		{"POST", "/api/profiles/a", "", 405},
		{"GET", "/api/unknown", "404 page not found\n", 404},
	} {
		w := httptest.NewRecorder()
		m.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status || w.Body.String() != tc.body {
			t.Fatalf("%s %s: %d %q", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}

func TestCompatCommandCancellation(t *testing.T) {
	a := &App{docker: "/bin/sh"}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	out, err := a.command(ctx, "-c", "echo ready; sleep 30 & wait").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "ready") || time.Since(start) > 3*time.Second {
		t.Fatalf("cancellation: %q %v (%v)", out, err, time.Since(start))
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if err := a.command(ctx, "-c", "exit 0").Run(); err != context.Canceled {
		t.Fatalf("pre-cancelled: %v", err)
	}
}

func TestCompatRootConfinement(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	putFile(t, filepath.Join(outside, "secret"), "original")
	root, err := openRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"escape/secret", "../secret", filepath.Join(outside, "secret")} {
		if f, err := root.OpenFile(name, os.O_WRONLY|os.O_TRUNC, 0); err == nil {
			f.Close()
			t.Fatalf("escaped: %s", name)
		}
	}
	if err := root.Mkdir("escape/new", 0755); err == nil {
		t.Fatal("mkdir escaped")
	}
	if err := root.Remove("escape/secret"); err == nil {
		t.Fatal("remove escaped")
	}
	if got := read(t, filepath.Join(outside, "secret")); got != "original" {
		t.Fatalf("outside changed: %q", got)
	}
	if err := writeThroughRoot(root, "nested/file", "inside"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested/file", filepath.Join(dir, "inside")); err != nil {
		t.Fatal(err)
	}
	if f, err := root.Open("inside"); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
	if err := root.Remove("nested/file"); err != nil {
		t.Fatal(err)
	}
}
