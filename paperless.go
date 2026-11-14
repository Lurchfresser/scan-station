package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"
)

// Paperless spricht mit der REST-API von Paperless-ngx. Hochgeladene
// Dokumente landen in der Warteschlange von Paperless; dort werden sie
// erkannt (OCR), gedreht, gerade gezogen und abgelegt.
type Paperless struct {
	url    string
	token  string
	client *http.Client
}

// So lange wird verfolgt, ob Paperless ein Dokument angenommen hat.
const paperlessFollow = 20 * time.Minute

func NewPaperless(cfg Config) *Paperless {
	if cfg.PaperlessURL == "" || cfg.PaperlessToken == "" {
		return nil
	}
	return &Paperless{
		url:   strings.TrimRight(cfg.PaperlessURL, "/"),
		token: cfg.PaperlessToken,
		// Grosszuegig: ein Stapel in Farbe kann einige 10 MB haben.
		client: &http.Client{Timeout: 5 * time.Minute},
	}
}

// CheckLogin prueft beim Start Adresse und Token.
func (p *Paperless) CheckLogin() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var out struct {
		Count int `json:"count"`
	}
	if err := p.getJSON(ctx, "/api/documents/?page_size=1&fields=id", &out); err != nil {
		log.Printf("WARNUNG: Paperless unter %s nicht nutzbar, Scans bleiben nur zum Download: %v", p.url, err)
		return
	}
	log.Printf("Paperless: verbunden mit %s (%d Dokumente)", p.url, out.Count)
}

// Upload schickt eine Datei an Paperless und gibt die Aufgaben-ID zurueck,
// unter der Paperless die Verarbeitung meldet.
func (p *Paperless) Upload(ctx context.Context, path, name string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// Datei direkt aus der Platte in die Anfrage streamen, ohne sie ganz in
	// den Speicher zu laden.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		part, err := mw.CreateFormFile("document", name)
		if err == nil {
			_, err = io.Copy(part, f)
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url+"/api/documents/post_document/", pr)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	p.auth(req)

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("Paperless nicht erreichbar: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("Paperless hat das Dokument abgelehnt (HTTP %d): %s", resp.StatusCode, firstLine(string(raw)))
	}
	// Die Antwort ist die Aufgaben-ID als JSON-String.
	var task string
	if err := json.Unmarshal(raw, &task); err != nil || task == "" {
		return "", fmt.Errorf("unerwartete Antwort von Paperless: %s", firstLine(string(raw)))
	}
	return task, nil
}

// PaperlessResult ist das Ergebnis der Verarbeitung in Paperless.
type PaperlessResult struct {
	OK      bool
	DocURL  string // Link auf das Dokument, wenn angenommen
	Message string // Begruendung, wenn abgelehnt
}

// Follow wartet, bis Paperless die Aufgabe abgeschlossen hat.
func (p *Paperless) Follow(ctx context.Context, task string) (PaperlessResult, error) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		var tasks []struct {
			Status          string  `json:"status"`
			Result          *string `json:"result"`
			RelatedDocument *string `json:"related_document"`
		}
		err := p.getJSON(ctx, "/api/tasks/?task_id="+task, &tasks)
		if err == nil && len(tasks) > 0 {
			t := tasks[0]
			msg := ""
			if t.Result != nil {
				msg = *t.Result
			}
			switch t.Status {
			case "SUCCESS":
				r := PaperlessResult{OK: true, Message: msg}
				if t.RelatedDocument != nil && *t.RelatedDocument != "" {
					r.DocURL = fmt.Sprintf("%s/documents/%s/details", p.url, *t.RelatedDocument)
				}
				return r, nil
			case "FAILURE", "REVOKED":
				return PaperlessResult{OK: false, Message: msg}, nil
			}
		}
		select {
		case <-ctx.Done():
			return PaperlessResult{}, ctx.Err()
		case <-tick.C:
		}
	}
}

func (p *Paperless) auth(req *http.Request) {
	req.Header.Set("Authorization", "Token "+p.token)
	req.Header.Set("Accept", "application/json")
}

func (p *Paperless) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url+path, nil)
	if err != nil {
		return err
	}
	p.auth(req)
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, firstLine(string(raw)))
	}
	return json.Unmarshal(raw, out)
}
