package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePaperless nimmt Dokumente an und meldet die Aufgabe beim ersten
// Abruf als laufend, beim zweiten als erledigt (oder abgelehnt).
type fakePaperless struct {
	mu       sync.Mutex
	uploads  map[string][]byte // Dateiname -> Inhalt
	polls    map[string]int
	reject   map[string]string // Dateiname -> Ablehnungsgrund
	taskFile map[string]string // Aufgabe -> Dateiname
}

func newFakePaperless() *fakePaperless {
	return &fakePaperless{uploads: map[string][]byte{}, polls: map[string]int{},
		reject: map[string]string{}, taskFile: map[string]string{}}
}

func (f *fakePaperless) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Token geheim" {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"detail":"Ungültiges Token."}`))
		return
	}
	switch {
	case r.URL.Path == "/api/documents/" && r.Method == http.MethodGet:
		w.Write([]byte(`{"count":42,"results":[]}`))
	case r.URL.Path == "/api/documents/post_document/" && r.Method == http.MethodPost:
		// Wie das echte Paperless (Django hinter Granian): ohne
		// Content-Length, also chunked, kommt dort keine Datei an.
		if r.ContentLength < 0 || len(r.TransferEncoding) > 0 {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"document":["No file was submitted."]}`))
			return
		}
		file, hdr, err := r.FormFile("document")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		data, _ := io.ReadAll(file)
		f.uploads[hdr.Filename] = data
		task := "task-" + hdr.Filename
		f.taskFile[task] = hdr.Filename
		json.NewEncoder(w).Encode(task)
	case r.URL.Path == "/api/tasks/":
		task := r.URL.Query().Get("task_id")
		name, ok := f.taskFile[task]
		if !ok {
			w.Write([]byte(`[]`))
			return
		}
		f.polls[task]++
		var out map[string]any
		switch {
		case f.polls[task] < 2:
			out = map[string]any{"status": "STARTED", "result": nil, "related_document": nil}
		case f.reject[name] != "":
			out = map[string]any{"status": "FAILURE", "result": f.reject[name], "related_document": nil}
		default:
			out = map[string]any{"status": "SUCCESS", "result": "Success. New document id 17 created", "related_document": "17"}
		}
		json.NewEncoder(w).Encode([]any{out})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestPaperless(t *testing.T) {
	fake := newFakePaperless()
	fake.reject["doppelt.pdf"] = "doppelt.pdf: Not consuming doppelt.pdf: It is a duplicate of x (#3)."
	srv := httptest.NewServer(fake)
	defer srv.Close()

	p := NewPaperless(Config{PaperlessURL: srv.URL + "/", PaperlessToken: "geheim"})
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, name := range []string{"scan.pdf", "doppelt.pdf"} {
		path := filepath.Join(dir, name)
		os.WriteFile(path, []byte("%PDF-1.4 "+name), 0o600)

		task, err := p.Upload(ctx, path, name)
		if err != nil {
			t.Fatal(err)
		}
		if string(fake.uploads[name]) != "%PDF-1.4 "+name {
			t.Fatalf("Inhalt von %s kam nicht an", name)
		}
		res, err := p.Follow(ctx, task)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s -> %+v", name, res)
		if name == "scan.pdf" && (!res.OK || res.DocURL != srv.URL+"/documents/17/details") {
			t.Fatalf("erwartet angenommen mit Link, bekam %+v", res)
		}
		if name == "doppelt.pdf" && (res.OK || !strings.Contains(res.Message, "duplicate")) {
			t.Fatalf("erwartet abgelehnt, bekam %+v", res)
		}
	}

	// Falscher Token: verstaendlicher Fehler.
	bad := NewPaperless(Config{PaperlessURL: srv.URL, PaperlessToken: "falsch"})
	path := filepath.Join(dir, "scan.pdf")
	if _, err := bad.Upload(ctx, path, "scan.pdf"); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("erwartet HTTP 401, bekam %v", err)
	}

	// Nicht konfiguriert: kein Client.
	if NewPaperless(Config{}) != nil {
		t.Fatal("Client ohne Konfiguration")
	}
}
