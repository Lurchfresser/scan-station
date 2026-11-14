package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Notifier schickt Nachrichten ueber einen Matrix-Bot. Jede Person bekommt
// einen eigenen Direktchat, den der Bot beim ersten Mal anlegt und sich in
// der Datenablage merkt. Fehler werden nur protokolliert: Ein nicht
// erreichbarer Matrix-Server darf keinen Scan stoeren.
type Notifier struct {
	homeserver string
	token      string
	users      map[string]string // Benutzer-ID der Station -> Matrix-ID
	publicURL  string
	client     *http.Client

	mu        sync.Mutex
	roomsFile string
	rooms     map[string]string // Matrix-ID -> Raum-ID
}

func NewNotifier(cfg Config) *Notifier {
	if cfg.MatrixHomeserver == "" || cfg.MatrixToken == "" || len(cfg.MatrixUsers) == 0 {
		return nil
	}
	n := &Notifier{
		homeserver: strings.TrimRight(cfg.MatrixHomeserver, "/"),
		token:      cfg.MatrixToken,
		users:      cfg.MatrixUsers,
		publicURL:  strings.TrimRight(cfg.PublicURL, "/"),
		client:     &http.Client{Timeout: 20 * time.Second},
		// Beginnt mit Punkt, damit es nie als Scan-Datei gilt.
		roomsFile: filepath.Join(cfg.DataDir, ".matrix-rooms.json"),
		rooms:     map[string]string{},
	}
	if raw, err := os.ReadFile(n.roomsFile); err == nil {
		if err := json.Unmarshal(raw, &n.rooms); err != nil {
			log.Printf("Matrix: %s unlesbar, Raeume werden neu angelegt: %v", n.roomsFile, err)
			n.rooms = map[string]string{}
		}
	}
	return n
}

// CheckLogin prueft beim Start, ob der Token gilt, und meldet den Bot.
func (n *Notifier) CheckLogin() {
	var who struct {
		UserID string `json:"user_id"`
	}
	if err := n.call(context.Background(), http.MethodGet, "/_matrix/client/v3/account/whoami", nil, &who); err != nil {
		log.Printf("WARNUNG: Matrix-Anmeldung fehlgeschlagen, Benachrichtigungen gehen nicht: %v", err)
		return
	}
	log.Printf("Matrix: angemeldet als %s, %d Person(en) mit Benachrichtigung", who.UserID, len(n.users))
}

// Notify schickt text an die Person mit der Stations-ID userID, im
// Hintergrund. Personen ohne Matrix-ID werden still uebergangen.
func (n *Notifier) Notify(userID, text string) {
	if n == nil {
		return
	}
	mxid, ok := n.users[userID]
	if !ok {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := n.send(ctx, mxid, text); err != nil {
			log.Printf("Matrix: Nachricht an %s fehlgeschlagen: %v", mxid, err)
		}
	}()
}

// link haengt die Adresse der Weboberflaeche an, falls bekannt.
func (n *Notifier) link() string {
	if n == nil || n.publicURL == "" {
		return ""
	}
	return "\n" + n.publicURL + "/"
}

func (n *Notifier) send(ctx context.Context, mxid, text string) error {
	// Nur ein Versand auf einmal, damit fuer eine Person nicht zwei Raeume
	// gleichzeitig entstehen.
	n.mu.Lock()
	defer n.mu.Unlock()

	for attempt := 0; attempt < 2; attempt++ {
		room, err := n.roomFor(ctx, mxid)
		if err != nil {
			return err
		}
		err = n.sendText(ctx, room, text)
		if err == nil {
			return nil
		}
		var me *matrixError
		if attempt == 0 && errors.As(err, &me) && (me.Code == "M_FORBIDDEN" || me.Code == "M_NOT_FOUND") {
			// Die Person hat den Chat verlassen oder der Bot ist nicht mehr
			// drin: neuen Raum anlegen und noch einmal versuchen.
			log.Printf("Matrix: Raum fuer %s nicht mehr nutzbar (%s), lege neu an", mxid, me.Code)
			delete(n.rooms, mxid)
			n.saveRooms()
			continue
		}
		return err
	}
	return errors.New("Versand nach neuem Raum erneut fehlgeschlagen")
}

func (n *Notifier) roomFor(ctx context.Context, mxid string) (string, error) {
	if room, ok := n.rooms[mxid]; ok {
		return room, nil
	}
	req := map[string]any{
		"preset":    "trusted_private_chat",
		"is_direct": true,
		"invite":    []string{mxid},
		"name":      "Scan-Station",
		"topic":     "Meldungen vom Scanner",
	}
	var resp struct {
		RoomID string `json:"room_id"`
	}
	if err := n.call(ctx, http.MethodPost, "/_matrix/client/v3/createRoom", req, &resp); err != nil {
		return "", fmt.Errorf("Raum anlegen: %w", err)
	}
	log.Printf("Matrix: Direktchat %s mit %s angelegt", resp.RoomID, mxid)
	n.rooms[mxid] = resp.RoomID
	n.saveRooms()
	return resp.RoomID, nil
}

func (n *Notifier) sendText(ctx context.Context, room, text string) error {
	// m.text statt m.notice: Hinweise von Bots (m.notice) loesen in vielen
	// Clients standardmaessig keine Benachrichtigung aus.
	body := map[string]string{"msgtype": "m.text", "body": text}
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/m.room.message/%s",
		url.PathEscape(room), url.PathEscape("scan-"+newToken(8)))
	return n.call(ctx, http.MethodPut, path, body, nil)
}

func (n *Notifier) saveRooms() {
	raw, _ := json.MarshalIndent(n.rooms, "", "  ")
	tmp := n.roomsFile + ".part"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("Matrix: Raumliste nicht gespeichert: %v", err)
		return
	}
	if err := os.Rename(tmp, n.roomsFile); err != nil {
		log.Printf("Matrix: Raumliste nicht gespeichert: %v", err)
	}
}

type matrixError struct {
	Status int
	Code   string `json:"errcode"`
	Msg    string `json:"error"`
}

func (e *matrixError) Error() string {
	return fmt.Sprintf("HTTP %d %s: %s", e.Status, e.Code, e.Msg)
}

func (n *Notifier) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, n.homeserver+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+n.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		me := &matrixError{Status: resp.StatusCode}
		_ = json.Unmarshal(raw, me)
		return me
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}
