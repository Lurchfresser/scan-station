package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// User ist eine Person, die scannen darf.
//
// Target bestimmt, wohin ein fertiger Scan geht:
//
//	"ocis"     -> wird mit rclone nach Remote hochgeladen und lokal geloescht
//	"download" -> bleibt auf dem Server liegen und ist ueber die Seite abrufbar
type User struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	PIN    string `json:"pin"`
	Target string `json:"target"`
	Remote string `json:"rclone_remote"`
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

func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()

	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}

	// Unbekannte Felder sollen auffallen, damit ein Tippfehler in der
	// Konfiguration nicht stillschweigend ignoriert wird.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}

	if len(cfg.Users) == 0 {
		return cfg, fmt.Errorf("%s: keine Benutzer konfiguriert", path)
	}

	seen := map[string]bool{}
	for i, u := range cfg.Users {
		if !validID.MatchString(u.ID) {
			return cfg, fmt.Errorf("%s: Benutzer %d: id %q darf nur Buchstaben, Ziffern, - und _ enthalten", path, i+1, u.ID)
		}
		if seen[u.ID] {
			return cfg, fmt.Errorf("%s: id %q kommt doppelt vor", path, u.ID)
		}
		seen[u.ID] = true

		if u.Name == "" {
			return cfg, fmt.Errorf("%s: Benutzer %q hat keinen Namen", path, u.ID)
		}
		if len(u.PIN) < 4 {
			return cfg, fmt.Errorf("%s: PIN von %q ist kuerzer als 4 Zeichen", path, u.ID)
		}
		switch u.Target {
		case "ocis":
			if u.Remote == "" {
				return cfg, fmt.Errorf("%s: %q hat target \"ocis\", aber kein rclone_remote", path, u.ID)
			}
		case "download":
			// nichts weiter noetig
		default:
			return cfg, fmt.Errorf("%s: %q hat unbekanntes target %q (erlaubt: ocis, download)", path, u.ID, u.Target)
		}
	}

	abs, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return cfg, err
	}
	cfg.DataDir = abs

	return cfg, nil
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
      "target": "ocis",
      "rclone_remote": "ocis:Scans"
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
