package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"time"
)

func main() {
	configPath := flag.String("config", "config.json", "Pfad zur Konfigurationsdatei (optional, SCAN_*-Variablen haben Vorrang)")
	initConfig := flag.Bool("init", false, "Beispielkonfiguration anlegen und beenden")
	flag.Parse()

	if *initConfig {
		if err := writeConfigTemplate(*configPath); err != nil {
			log.Fatalf("Vorlage konnte nicht geschrieben werden: %v", err)
		}
		fmt.Printf("Vorlage angelegt: %s\nBitte Namen und PINs anpassen.\n", *configPath)
		return
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("Konfiguration: %v", err)
	}

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		log.Fatalf("Datenverzeichnis %s: %v", cfg.DataDir, err)
	}

	checkTools(cfg)

	station := NewStation(cfg)
	station.startCleanupLoop()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           NewServer(cfg, station).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// Ein Scan laeuft im Hintergrund, die Antworten selbst sind kurz.
		WriteTimeout: 60 * time.Second,
	}

	log.Printf("Scanner: %s", cfg.Scanner)
	log.Printf("Ablage:  %s (Aufbewahrung: %d Tage)", cfg.DataDir, cfg.RetentionDays)
	for _, u := range cfg.Users {
		if u.Target == "ocis" {
			log.Printf("Benutzer %-10s -> Cloud (%s)", u.Name, u.Remote)
		} else {
			log.Printf("Benutzer %-10s -> Download", u.Name)
		}
	}
	for _, addr := range localURLs(cfg.Listen) {
		log.Printf("Erreichbar unter %s", addr)
	}

	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// checkTools warnt beim Start, wenn ein benoetigtes Programm fehlt. Der Dienst
// startet trotzdem, damit man die Oberflaeche auch ohne Scanner ansehen kann.
func checkTools(cfg Config) {
	required := []string{"scanimage", "img2pdf"}
	for _, u := range cfg.Users {
		if u.Target == "ocis" {
			required = append(required, "rclone")
			break
		}
	}
	for _, tool := range required {
		if _, err := exec.LookPath(tool); err != nil {
			log.Printf("WARNUNG: %s ist nicht installiert, Scans werden fehlschlagen.", tool)
		}
	}
	if _, err := exec.LookPath("ocrmypdf"); err != nil {
		log.Printf("Hinweis: ocrmypdf fehlt, die PDFs werden nicht durchsuchbar.")
	}
}

// localURLs baut aus der Listen-Adresse anklickbare Adressen fuer das Heimnetz.
func localURLs(listen string) []string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return []string{"http://localhost" + listen}
	}
	if host != "" && host != "0.0.0.0" && host != "::" {
		return []string{fmt.Sprintf("http://%s:%s", host, port)}
	}

	urls := []string{fmt.Sprintf("http://localhost:%s", port)}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return urls
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		if ip4 := ipnet.IP.To4(); ip4 != nil {
			urls = append(urls, fmt.Sprintf("http://%s:%s", ip4, port))
		}
	}
	return urls
}
