package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeMatrix ist ein minimaler Homeserver: createRoom, send und whoami.
type fakeMatrix struct {
	mu        sync.Mutex
	created   int
	sent      []string // "raum|text"
	forbidden map[string]bool
}

func (f *fakeMatrix) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer geheim" {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"errcode":"M_UNKNOWN_TOKEN","error":"bad token"}`))
		return
	}
	switch {
	case r.URL.Path == "/_matrix/client/v3/account/whoami":
		w.Write([]byte(`{"user_id":"@scanbot:example.org"}`))
	case r.URL.Path == "/_matrix/client/v3/createRoom":
		var req struct {
			Invite   []string `json:"invite"`
			IsDirect bool     `json:"is_direct"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if len(req.Invite) != 1 || !req.IsDirect {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.created++
		json.NewEncoder(w).Encode(map[string]string{"room_id": "!raum" + string(rune('0'+f.created)) + ":example.org"})
	case strings.Contains(r.URL.Path, "/send/m.room.message/"):
		room := strings.Split(strings.TrimPrefix(r.URL.Path, "/_matrix/client/v3/rooms/"), "/")[0]
		if f.forbidden[room] {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"errcode":"M_FORBIDDEN","error":"not in room"}`))
			return
		}
		var msg map[string]string
		json.NewDecoder(r.Body).Decode(&msg)
		if msg["msgtype"] != "m.text" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.sent = append(f.sent, room+"|"+msg["body"])
		w.Write([]byte(`{"event_id":"$x"}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestNotifier(t *testing.T) {
	fake := &fakeMatrix{forbidden: map[string]bool{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	cfg := Config{
		DataDir:          t.TempDir(),
		MatrixHomeserver: srv.URL,
		MatrixToken:      "geheim",
		MatrixUsers:      map[string]string{"anna": "@anna:example.org"},
	}
	n := NewNotifier(cfg)
	ctx := context.Background()

	// Erste Nachricht legt den Raum an, die zweite benutzt ihn wieder.
	for _, txt := range []string{"eins", "zwei"} {
		if err := n.send(ctx, "@anna:example.org", txt); err != nil {
			t.Fatal(err)
		}
	}
	if fake.created != 1 || len(fake.sent) != 2 {
		t.Fatalf("created=%d sent=%v", fake.created, fake.sent)
	}

	// Raumliste ueberlebt einen Neustart.
	if n2 := NewNotifier(cfg); n2.rooms["@anna:example.org"] != "!raum1:example.org" {
		t.Fatalf("Raumliste nicht gespeichert: %v", n2.rooms)
	}

	// Person hat den Chat verlassen: neuer Raum, Nachricht kommt trotzdem an.
	fake.forbidden["!raum1:example.org"] = true
	if err := n.send(ctx, "@anna:example.org", "drei"); err != nil {
		t.Fatal(err)
	}
	if fake.created != 2 || fake.sent[len(fake.sent)-1] != "!raum2:example.org|drei" {
		t.Fatalf("created=%d sent=%v", fake.created, fake.sent)
	}

	// Falscher Token: Fehler, kein Absturz.
	bad := NewNotifier(Config{DataDir: t.TempDir(), MatrixHomeserver: srv.URL, MatrixToken: "falsch",
		MatrixUsers: map[string]string{"anna": "@anna:example.org"}})
	if err := bad.send(ctx, "@anna:example.org", "x"); err == nil || !strings.Contains(err.Error(), "M_UNKNOWN_TOKEN") {
		t.Fatalf("erwartet M_UNKNOWN_TOKEN, bekam %v", err)
	}

	// Ohne Konfiguration gibt es keinen Notifier, und Aufrufe sind harmlos.
	var none *Notifier = NewNotifier(Config{})
	none.Notify("anna", "x")
	if none.link() != "" {
		t.Fatal("link ohne Notifier")
	}
}

func TestResultMessage(t *testing.T) {
	ok := resultMessage(&Job{State: StateDone, Pages: 3, File: "a.pdf", Note: "1 leere Seite(n) entfernt."}, ScanOptions{Output: outputPDF})
	if ok != "✅ Scan fertig: 3 Seite(n), a.pdf\n1 leere Seite(n) entfernt." {
		t.Fatalf("%q", ok)
	}
	bad := resultMessage(&Job{State: StateError, Note: "Papierstau im Einzug."}, ScanOptions{})
	if bad != "❌ Scan fehlgeschlagen: Papierstau im Einzug." {
		t.Fatalf("%q", bad)
	}
}

func TestMatrixUsersEnv(t *testing.T) {
	m, err := parseMatrixUsers("anna=@anna:example.org, ben = @ben:matrix.example.org:8448")
	if err != nil || m["anna"] != "@anna:example.org" || m["ben"] != "@ben:matrix.example.org:8448" {
		t.Fatalf("%v %v", m, err)
	}
	if _, err := parseMatrixUsers("anna@anna:example.org"); err == nil {
		t.Fatal("fehlendes = nicht erkannt")
	}
	for _, id := range []string{"@anna:example.org", "@ben:matrix.example.org:8448"} {
		if !validMXID.MatchString(id) {
			t.Errorf("%s abgelehnt", id)
		}
	}
	if validMXID.MatchString("anna:example.org") {
		t.Error("ID ohne @ angenommen")
	}
}
