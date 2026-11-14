package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// User ist eine Person, die scannen darf.
//
// Target bestimmt, wohin ein fertiger Scan geht:
//
//	"download"  -> bleibt auf dem Server liegen und ist ueber die Seite abrufbar
//	"paperless" -> geht an Paperless-ngx; eine Kopie bleibt bis zum Ablauf der
//	               Aufbewahrungsfrist zum Download liegen
type User struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	PIN    string `json:"pin"`
	Target string `json:"target"`
}

type Config struct {
	Scanner       string `json:"scanner"`
	Listen        string `json:"listen"`
	DataDir       string `json:"data_dir"`
	RetentionDays int    `json:"retention_days"`
	OCRLang       string `json:"ocr_lang"`
	PaperWidthMM  int    `json:"paper_width_mm"`
	PaperHeightMM int    `json:"paper_height_mm"`
	Users         []User `json:"users"`

	// TrustProxy: der Dienst laeuft hinter einem Reverse Proxy (Traefik).
	// Dann gilt der letzte Eintrag in X-Forwarded-For als Adresse des
	// Aufrufers, sonst saehe die Anmeldesperre alle Anfragen von derselben
	// Proxy-Adresse kommen. Nur einschalten, wenn der Dienst ausschliesslich
	// ueber den Proxy erreichbar ist, sonst laesst sich die Kopfzeile faelschen.
	TrustProxy bool `json:"trust_proxy"`

	// Oeffentliche Adresse der Weboberflaeche, fuer Links in Nachrichten.
	PublicURL string `json:"public_url"`

	// Paperless-ngx: Adresse und API-Token (Profil -> API-Token).
	PaperlessURL   string `json:"paperless_url"`
	PaperlessToken string `json:"paperless_token"`

	// Matrix-Benachrichtigungen. Nur aktiv, wenn alle drei gesetzt sind.
	MatrixHomeserver string            `json:"matrix_homeserver"`
	MatrixToken      string            `json:"matrix_token"`
	MatrixUsers      map[string]string `json:"matrix_users"` // Stations-ID -> @name:server
}

func defaultConfig() Config {
	return Config{
		Scanner:       "brother2:net1;dev0",
		Listen:        ":8080",
		DataDir:       "./data",
		RetentionDays: 7,
		OCRLang:       "deu",
		PaperWidthMM:  210,
		PaperHeightMM: 297,
	}
}

// loadConfig liest die Konfiguration in dieser Reihenfolge:
//
//  1. eingebaute Standardwerte
//  2. die JSON-Datei unter path, falls vorhanden (optional)
//  3. Umgebungsvariablen SCAN_*, die alles Vorherige ueberschreiben
//
// So laesst sich der Dienst komplett ueber Umgebungsvariablen steuern, etwa
// aus dem Environment-Tab von Dokploy.
func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()

	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		// Unbekannte Felder sollen auffallen, damit ein Tippfehler in der
		// Konfiguration nicht stillschweigend ignoriert wird.
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("%s: %w", path, err)
		}
	case os.IsNotExist(err):
		// Ohne Datei muss alles aus der Umgebung kommen.
	default:
		return cfg, err
	}

	if err := applyEnv(&cfg, os.LookupEnv); err != nil {
		return cfg, err
	}

	if len(cfg.Users) == 0 {
		return cfg, errors.New("keine Benutzer konfiguriert: SCAN_USERS setzen " +
			"(z.B. SCAN_USERS=\"anna:Anna:1234,ben:Ben:5678\") oder eine Konfigurationsdatei anlegen")
	}

	seen := map[string]bool{}
	for i, u := range cfg.Users {
		if !validID.MatchString(u.ID) {
			return cfg, fmt.Errorf("Benutzer %d: id %q darf nur Buchstaben, Ziffern, - und _ enthalten", i+1, u.ID)
		}
		if seen[u.ID] {
			return cfg, fmt.Errorf("id %q kommt doppelt vor", u.ID)
		}
		seen[u.ID] = true

		if u.Name == "" {
			return cfg, fmt.Errorf("Benutzer %q hat keinen Namen", u.ID)
		}
		if len(u.PIN) < 4 {
			return cfg, fmt.Errorf("PIN von %q ist kuerzer als 4 Zeichen", u.ID)
		}
		switch u.Target {
		case "paperless":
			if cfg.PaperlessURL == "" || cfg.PaperlessToken == "" {
				return cfg, fmt.Errorf("%q hat target \"paperless\", aber PAPERLESS_URL oder PAPERLESS_TOKEN fehlt", u.ID)
			}
		case "download":
			// nichts weiter noetig
		default:
			return cfg, fmt.Errorf("%q hat unbekanntes target %q (erlaubt: download, paperless)", u.ID, u.Target)
		}
	}

	for id, mxid := range cfg.MatrixUsers {
		if !seen[id] {
			return cfg, fmt.Errorf("Matrix: %q ist keine bekannte Benutzer-ID", id)
		}
		if !validMXID.MatchString(mxid) {
			return cfg, fmt.Errorf("Matrix: %q ist keine gueltige Matrix-ID (erwartet @name:server)", mxid)
		}
	}
	if cfg.PaperlessURL != "" && !strings.HasPrefix(cfg.PaperlessURL, "http") {
		return cfg, fmt.Errorf("PAPERLESS_URL muss mit http:// oder https:// beginnen")
	}
	if cfg.MatrixHomeserver != "" && !strings.HasPrefix(cfg.MatrixHomeserver, "http") {
		return cfg, fmt.Errorf("MATRIX_HOMESERVER muss mit http:// oder https:// beginnen")
	}

	abs, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return cfg, err
	}
	cfg.DataDir = abs

	return cfg, nil
}

// applyEnv uebernimmt gesetzte SCAN_*-Variablen. lookup ist os.LookupEnv,
// als Parameter, damit es sich ohne echte Umgebung testen laesst.
func applyEnv(cfg *Config, lookup func(string) (string, bool)) error {
	str := func(key string, dst *string) {
		if v, ok := lookup(key); ok && strings.TrimSpace(v) != "" {
			*dst = strings.TrimSpace(v)
		}
	}
	num := func(key string, dst *int) error {
		if v, ok := lookup(key); ok && strings.TrimSpace(v) != "" {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return fmt.Errorf("%s=%q ist keine Zahl", key, v)
			}
			*dst = n
		}
		return nil
	}
	flag := func(key string, dst *bool) error {
		if v, ok := lookup(key); ok && strings.TrimSpace(v) != "" {
			b, err := strconv.ParseBool(strings.TrimSpace(v))
			if err != nil {
				return fmt.Errorf("%s=%q: erlaubt sind true oder false", key, v)
			}
			*dst = b
		}
		return nil
	}

	str("SCAN_SCANNER", &cfg.Scanner)
	str("SCAN_LISTEN", &cfg.Listen)
	str("SCAN_DATA_DIR", &cfg.DataDir)
	str("SCAN_OCR_LANG", &cfg.OCRLang)
	for key, dst := range map[string]*int{
		"SCAN_RETENTION_DAYS":  &cfg.RetentionDays,
		"SCAN_PAPER_WIDTH_MM":  &cfg.PaperWidthMM,
		"SCAN_PAPER_HEIGHT_MM": &cfg.PaperHeightMM,
	} {
		if err := num(key, dst); err != nil {
			return err
		}
	}
	if err := flag("SCAN_TRUST_PROXY", &cfg.TrustProxy); err != nil {
		return err
	}
	str("SCAN_PUBLIC_URL", &cfg.PublicURL)
	str("PAPERLESS_URL", &cfg.PaperlessURL)
	str("PAPERLESS_TOKEN", &cfg.PaperlessToken)
	str("MATRIX_HOMESERVER", &cfg.MatrixHomeserver)
	str("MATRIX_TOKEN", &cfg.MatrixToken)
	if v, ok := lookup("SCAN_MATRIX_USERS"); ok && strings.TrimSpace(v) != "" {
		m, err := parseMatrixUsers(v)
		if err != nil {
			return fmt.Errorf("SCAN_MATRIX_USERS: %w", err)
		}
		cfg.MatrixUsers = m
	}

	if v, ok := lookup("SCAN_USERS"); ok && strings.TrimSpace(v) != "" {
		users, err := parseUsers(v)
		if err != nil {
			return fmt.Errorf("SCAN_USERS: %w", err)
		}
		cfg.Users = users
	}
	return nil
}

// parseMatrixUsers liest SCAN_MATRIX_USERS: id=@name:server, getrennt durch
// Komma. Eigene Variable, weil Matrix-IDs selbst Doppelpunkte enthalten.
func parseMatrixUsers(s string) (map[string]string, error) {
	m := map[string]string{}
	for i, entry := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == ';' }) {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		id, mxid, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("Eintrag %d: erwartet id=@name:server", i+1)
		}
		m[strings.TrimSpace(id)] = strings.TrimSpace(mxid)
	}
	return m, nil
}

// parseUsers liest SCAN_USERS. Personen sind durch Komma (oder Zeilenumbruch)
// getrennt, die Felder durch Doppelpunkt:
//
//	id:Name:PIN[:target]
//
// target ist optional und standardmaessig "download". Name darf Leerzeichen
// enthalten, PIN und Name keinen Doppelpunkt und kein Komma.
func parseUsers(s string) ([]User, error) {
	var users []User
	for i, entry := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == ';' }) {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) < 3 || len(parts) > 4 {
			return nil, fmt.Errorf("Eintrag %d: erwartet id:Name:PIN oder id:Name:PIN:target", i+1)
		}
		u := User{
			ID:     strings.TrimSpace(parts[0]),
			Name:   strings.TrimSpace(parts[1]),
			PIN:    strings.TrimSpace(parts[2]),
			Target: "download",
		}
		if len(parts) == 4 && strings.TrimSpace(parts[3]) != "" {
			u.Target = strings.TrimSpace(parts[3])
		}
		users = append(users, u)
	}
	return users, nil
}

func (c Config) userByID(id string) (User, bool) {
	for _, u := range c.Users {
		if u.ID == id {
			return u, true
		}
	}
	return User{}, false
}

const configTemplate = `{
  "scanner": "brother2:net1;dev0",
  "listen": ":8080",
  "data_dir": "./data",
  "retention_days": 7,
  "ocr_lang": "deu",
  "paper_width_mm": 210,
  "paper_height_mm": 297,
  "users": [
    {
      "id": "ich",
      "name": "Ich",
      "pin": "BITTE-AENDERN",
      "target": "download"
    },
    {
      "id": "anna",
      "name": "Anna",
      "pin": "BITTE-AENDERN",
      "target": "download"
    },
    {
      "id": "ben",
      "name": "Ben",
      "pin": "BITTE-AENDERN",
      "target": "download"
    }
  ]
}
`

func writeConfigTemplate(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s existiert bereits", path)
	}
	return os.WriteFile(path, []byte(configTemplate), 0o600)
}
