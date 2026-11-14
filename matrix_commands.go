package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// Befehle per Matrix: Der Bot liest in seinen Direktchats mit und nimmt
// kurze Antworten entgegen, etwa "weiter", wenn der Stapel umgedreht ist.
// Befehle zaehlen nur von der Person, der der Chat gehoert.

// CommandHandler fuehrt einen Befehl fuer die Person userID aus und gibt die
// Antwort zurueck.
type CommandHandler func(userID, cmd string) string

// Filter fuer /sync: nur Nachrichten, keine Anwesenheit, keine Mitgliederlisten.
const syncFilter = `{"presence":{"types":[]},"account_data":{"types":[]},` +
	`"room":{"timeline":{"types":["m.room.message","m.room.encrypted"],"limit":20},` +
	`"state":{"types":[]},"ephemeral":{"types":[]},"account_data":{"types":[]}}}`

// Listen liest die Chats des Bots mit, bis ctx endet.
func (n *Notifier) Listen(ctx context.Context, handle CommandHandler) {
	if n == nil {
		return
	}
	// Eigener Client: /sync haelt die Verbindung bis zu 30 s offen.
	syncClient := &http.Client{Timeout: 60 * time.Second}

	var who struct {
		UserID string `json:"user_id"`
	}
	for {
		if err := n.call(ctx, http.MethodGet, "/_matrix/client/v3/account/whoami", nil, &who); err == nil {
			break
		}
		if !sleepCtx(ctx, time.Minute) {
			return
		}
	}

	since := ""
	warnedEncrypted := map[string]bool{}
	for ctx.Err() == nil {
		q := url.Values{"filter": {syncFilter}, "timeout": {"30000"}}
		if since != "" {
			q.Set("since", since)
		} else {
			q.Set("timeout", "0")
		}
		var resp syncResponse
		err := n.callWith(ctx, syncClient, http.MethodGet, "/_matrix/client/v3/sync?"+q.Encode(), nil, &resp)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("Matrix: Mitlesen unterbrochen, neuer Versuch in 15 s: %v", err)
			}
			if !sleepCtx(ctx, 15*time.Second) {
				return
			}
			continue
		}
		first := since == ""
		since = resp.NextBatch
		if first {
			// Beim Start nichts Altes ausfuehren, nur ab jetzt mitlesen.
			log.Printf("Matrix: lese Antworten im Direktchat mit (weiter, fertig, abbrechen, status)")
			continue
		}

		for roomID, room := range resp.Rooms.Join {
			for _, ev := range room.Timeline.Events {
				if ev.Sender == who.UserID {
					continue
				}
				userID, ok := n.ownerOf(roomID, ev.Sender)
				if !ok {
					continue
				}
				if ev.Type == "m.room.encrypted" {
					if !warnedEncrypted[roomID] {
						warnedEncrypted[roomID] = true
						n.reply(ctx, roomID, "🔒 Dieser Chat ist verschlüsselt, deine Antworten kann ich nicht lesen. "+
							"Verlasse den Chat; ich lege beim nächsten Scan einen neuen, unverschlüsselten an.")
					}
					continue
				}
				if ev.Content.MsgType != "m.text" {
					continue
				}
				cmd := commandWord(ev.Content.Body)
				if cmd == "" {
					continue
				}
				log.Printf("Matrix: Befehl %q von %s", cmd, ev.Sender)
				n.reply(ctx, roomID, handle(userID, cmd))
			}
		}
	}
}

type syncResponse struct {
	NextBatch string `json:"next_batch"`
	Rooms     struct {
		Join map[string]struct {
			Timeline struct {
				Events []struct {
					Type    string `json:"type"`
					Sender  string `json:"sender"`
					Content struct {
						MsgType string `json:"msgtype"`
						Body    string `json:"body"`
					} `json:"content"`
				} `json:"events"`
			} `json:"timeline"`
		} `json:"join"`
	} `json:"rooms"`
}

// ownerOf liefert die Stations-ID, wenn roomID der Direktchat von sender ist.
func (n *Notifier) ownerOf(roomID, sender string) (string, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.rooms[sender] != roomID {
		return "", false
	}
	for id, mxid := range n.users {
		if mxid == sender {
			return id, true
		}
	}
	return "", false
}

func (n *Notifier) reply(ctx context.Context, roomID, text string) {
	if text == "" {
		return
	}
	if err := n.sendText(ctx, roomID, text); err != nil {
		log.Printf("Matrix: Antwort fehlgeschlagen: %v", err)
	}
}

// commandWord macht aus einer Chatnachricht ein Befehlswort: Zitate einer
// Antwort ("> ...") weglassen, erstes Wort, klein, ohne Satzzeichen.
func commandWord(body string) string {
	var lines []string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), ">") {
			continue
		}
		lines = append(lines, l)
	}
	f := strings.Fields(strings.ToLower(strings.Join(lines, " ")))
	if len(f) == 0 {
		return ""
	}
	return strings.TrimFunc(f[0], func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// HandleCommand setzt Matrix-Befehle fuer die Station um.
func (s *Station) HandleCommand(userID, cmd string) string {
	switch cmd {
	case "weiter", "w", "ja", "ok", "rückseiten", "rueckseiten", "go", "los":
		if s.DuplexAction(userID, duplexContinue) {
			return "▶️ Rückseiten werden gescannt."
		}
		return "Es wartet gerade kein Scan von dir auf die Rückseiten."
	case "fertig", "ohne", "nein", "skip":
		if s.DuplexAction(userID, duplexSkip) {
			return "⏹️ Wird ohne Rückseiten fertiggestellt."
		}
		return "Es wartet gerade kein Scan von dir auf die Rückseiten."
	case "abbrechen", "abbruch", "stop", "stopp", "cancel":
		job := s.Snapshot()
		if job.State != StateRunning || job.UserID != userID {
			return "Du hast gerade keinen laufenden Scan."
		}
		s.Cancel()
		return "🛑 Scan abgebrochen."
	case "status":
		job := s.Snapshot()
		switch {
		case job.State != StateRunning:
			return "Der Scanner ist frei."
		case job.UserID != userID:
			return fmt.Sprintf("%s scannt gerade.", job.UserName)
		case job.Waiting:
			return fmt.Sprintf("Wartet aufs Umdrehen (%d Vorderseiten). Antworte „weiter“ oder „fertig“.", job.Fronts)
		default:
			return fmt.Sprintf("%s (%d %%), bisher %d Seite(n).", job.Step, job.Progress, job.Pages)
		}
	case "hilfe", "help", "befehle", "":
		return "Befehle: „weiter“ (Rückseiten scannen), „fertig“ (ohne Rückseiten), " +
			"„abbrechen“, „status“."
	}
	return "Das verstehe ich nicht. Schreib „hilfe“ für die Befehle."
}
