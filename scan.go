package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Ein Auftrag darf hoechstens so lange laufen. Das Geraet braucht pro Seite
// mehrere Minuten, deshalb ist die Grenze bewusst hoch.
const jobTimeout = 90 * time.Minute

type JobState string

const (
	StateIdle    JobState = "idle"
	StateRunning JobState = "running"
	StateDone    JobState = "done"
	StateError   JobState = "error"
)

type ScanOptions struct {
	Mode       string // z.B. "True Gray"
	Resolution int
	Source     string // "Automatic Document Feeder" oder "FlatBed"
	Prefix     string
}

type Job struct {
	ID       string    `json:"id"`
	UserID   string    `json:"user_id"`
	UserName string    `json:"user_name"`
	State    JobState  `json:"state"`
	Step     string    `json:"step"`
	Progress int       `json:"progress"`
	Pages    int       `json:"pages"`
	File     string    `json:"file"`
	Target   string    `json:"target"`
	Note     string    `json:"note"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
}

// Station fuehrt die Scanauftraege aus. Das Geraet kann nur einen Auftrag
// gleichzeitig bedienen, deshalb laeuft hier immer hoechstens einer.
type Station struct {
	cfg Config

	mu     sync.Mutex
	job    *Job
	busy   bool
	cancel context.CancelFunc
}

func NewStation(cfg Config) *Station {
	return &Station{cfg: cfg}
}

// Snapshot gibt eine Kopie des aktuellen oder letzten Auftrags zurueck.
func (s *Station) Snapshot() Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil {
		return Job{State: StateIdle}
	}
	return *s.job
}

func (s *Station) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy
}

var errBusy = errors.New("es laeuft bereits ein Scan")

func (s *Station) Start(u User, opts ScanOptions) error {
	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		return errBusy
	}

	ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
	job := &Job{
		ID:       newToken(8),
		UserID:   u.ID,
		UserName: u.Name,
		State:    StateRunning,
		Step:     "Scanner aufwaermen",
		Target:   u.Target,
		Started:  time.Now(),
	}
	s.job = job
	s.busy = true
	s.cancel = cancel
	s.mu.Unlock()

	go func() {
		defer cancel()
		err := s.run(ctx, job, u, opts)

		s.mu.Lock()
		defer s.mu.Unlock()
		job.Finished = time.Now()
		job.Progress = 0
		if err != nil {
			job.State = StateError
			job.Step = ""
			job.Note = err.Error()
			log.Printf("Auftrag %s (%s) fehlgeschlagen: %v", job.ID, u.ID, err)
		} else {
			job.State = StateDone
			job.Step = ""
			log.Printf("Auftrag %s (%s) fertig: %d Seite(n)", job.ID, u.ID, job.Pages)
		}
		s.busy = false
		s.cancel = nil
	}()

	return nil
}

// Cancel bricht einen laufenden Auftrag ab.
func (s *Station) Cancel() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.busy || s.cancel == nil {
		return false
	}
	s.cancel()
	if s.job != nil {
		s.job.Note = "Abgebrochen."
	}
	return true
}

func (s *Station) setStep(job *Job, step string, progress int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job.Step = step
	job.Progress = progress
}

func (s *Station) setPages(job *Job, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job.Pages = n
}

func (s *Station) run(ctx context.Context, job *Job, u User, opts ScanOptions) error {
	workdir, err := os.MkdirTemp("", "scanstation-")
	if err != nil {
		return fmt.Errorf("Arbeitsverzeichnis konnte nicht angelegt werden: %w", err)
	}
	defer os.RemoveAll(workdir)

	pages, err := s.scanPages(ctx, job, workdir, opts)
	if err != nil {
		return err
	}
	s.setPages(job, len(pages))

	name := fmt.Sprintf("%s_%s.pdf", opts.Prefix, job.Started.Format("2006-01-02_1504"))
	pdf := filepath.Join(workdir, name)

	s.setStep(job, "PDF erstellen", 0)
	if err := s.makePDF(ctx, pages, pdf); err != nil {
		return err
	}

	if _, err := exec.LookPath("ocrmypdf"); err == nil {
		s.setStep(job, "Texterkennung", 0)
		if err := s.runOCR(ctx, pdf); err != nil {
			// Ohne Texterkennung ist das PDF weiterhin brauchbar.
			log.Printf("Auftrag %s: OCR uebersprungen: %v", job.ID, err)
			s.mu.Lock()
			job.Note = "Hinweis: Die Texterkennung ist fehlgeschlagen, das PDF enthaelt nur Bilder."
			s.mu.Unlock()
		}
	}

	switch u.Target {
	case "ocis":
		s.setStep(job, "Upload in die Cloud", 0)
		if err := s.upload(ctx, pdf, u.Remote); err != nil {
			// Der Scan ist zu wertvoll, um ihn zu verlieren: bei einem
			// fehlgeschlagenen Upload bleibt er lokal abrufbar.
			if saveErr := s.store(pdf, u.ID, name); saveErr != nil {
				return fmt.Errorf("%v (auch die lokale Sicherung schlug fehl: %v)", err, saveErr)
			}
			s.mu.Lock()
			job.File = name
			job.Target = "download"
			s.mu.Unlock()
			return fmt.Errorf("%v Der Scan liegt stattdessen hier zum Download bereit", err)
		}
		s.mu.Lock()
		job.Note = fmt.Sprintf("In der Cloud gespeichert: %s/%s", u.Remote, name)
		s.mu.Unlock()

	default:
		s.setStep(job, "Ablegen", 0)
		if err := s.store(pdf, u.ID, name); err != nil {
			return err
		}
		s.mu.Lock()
		job.File = name
		s.mu.Unlock()
	}

	return nil
}

var progressRe = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)%`)

func (s *Station) scanPages(ctx context.Context, job *Job, workdir string, opts ScanOptions) ([]string, error) {
	args := []string{
		"-d", s.cfg.Scanner,
		"--source", opts.Source,
		"--mode", opts.Mode,
		"--resolution", fmt.Sprint(opts.Resolution),
		"-l", "0", "-t", "0",
		"-x", fmt.Sprint(s.cfg.PaperWidthMM),
		"-y", fmt.Sprint(s.cfg.PaperHeightMM),
		"--format=png",
		"--progress",
	}
	if opts.Source == sourceFlatbed {
		args = append(args, "-o", filepath.Join(workdir, "page_001.png"))
	} else {
		args = append(args, "--batch="+filepath.Join(workdir, "page_%03d.png"), "--batch-print")
	}

	cmd := exec.CommandContext(ctx, "scanimage", args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("scanimage konnte nicht gestartet werden: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		io.Copy(io.Discard, stdout)
	}()

	// scanimage schreibt Fortschritt und Fehler nach stderr. Die letzten
	// Zeilen werden aufbewahrt, um im Fehlerfall etwas sagen zu koennen.
	var tail []string
	sc := bufio.NewScanner(stderr)
	sc.Split(scanLinesAndCR)
	pagesSeen := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if m := progressRe.FindStringSubmatch(line); m != nil {
			var p float64
			fmt.Sscanf(m[1], "%f", &p)
			s.setStep(job, fmt.Sprintf("Seite %d scannen", pagesSeen+1), int(p))
			continue
		}
		if strings.Contains(line, "Scanning page") {
			pagesSeen++
			s.setStep(job, fmt.Sprintf("Seite %d scannen", pagesSeen), 0)
			s.setPages(job, pagesSeen)
			continue
		}
		tail = append(tail, line)
		if len(tail) > 12 {
			tail = tail[1:]
		}
	}
	waitErr := cmd.Wait()
	wg.Wait()

	pages, globErr := filepath.Glob(filepath.Join(workdir, "page_*.png"))
	if globErr != nil {
		return nil, globErr
	}
	// Teilweise geschriebene Dateien am Ende eines Stapels verwerfen.
	var good []string
	for _, p := range pages {
		if st, err := os.Stat(p); err == nil && st.Size() > 0 {
			good = append(good, p)
		} else {
			os.Remove(p)
		}
	}
	sort.Strings(good)

	if len(good) > 0 {
		// Beim Stapelscan endet scanimage regulaer mit einem Fehler, sobald
		// der Einzug leer ist. Solange Seiten entstanden sind, ist das in
		// Ordnung.
		return good, nil
	}

	if ctx.Err() != nil {
		return nil, errors.New("Der Scan wurde abgebrochen oder hat zu lange gedauert.")
	}
	return nil, fmt.Errorf("Es wurde keine Seite gescannt. %s", explainScanError(strings.Join(tail, " "), waitErr))
}

// explainScanError macht aus den Meldungen von scanimage einen Satz, mit dem
// man am Geraet etwas anfangen kann.
func explainScanError(out string, waitErr error) string {
	low := strings.ToLower(out)
	switch {
	case strings.Contains(low, "cover is open"), strings.Contains(low, "cover open"):
		return "Die Klappe am Gerät ist offen. Bitte schließen und erneut versuchen."
	case strings.Contains(low, "out of documents"), strings.Contains(low, "no documents"):
		return "Der Vorlageneinzug ist leer. Legen Sie die Blätter ein, oder wählen Sie das Flachbett."
	case strings.Contains(low, "document feeder jammed"), strings.Contains(low, "jam"):
		return "Papierstau im Einzug. Bitte das Blatt entfernen."
	case strings.Contains(low, "device busy"), strings.Contains(low, "busy"):
		return "Das Gerät ist beschäftigt. Warten Sie einen Moment und versuchen Sie es erneut."
	case strings.Contains(low, "invalid argument"):
		return "Das Gerät hat die Einstellungen abgelehnt. Versuchen Sie eine niedrigere Auflösung."
	case strings.Contains(low, "error during device i/o"), strings.Contains(low, "network"):
		return "Keine Verbindung zum Scanner. Prüfen Sie, ob das Gerät eingeschaltet und im Netz ist."
	case out != "":
		return "Meldung des Scanners: " + firstLine(out)
	case waitErr != nil:
		return "scanimage endete mit: " + waitErr.Error()
	}
	return "Keine weitere Meldung vorhanden."
}

func (s *Station) makePDF(ctx context.Context, pages []string, out string) error {
	args := append([]string{}, pages...)
	args = append(args, "-o", out)
	cmd := exec.CommandContext(ctx, "img2pdf", args...)
	if msg, err := runCaptured(cmd); err != nil {
		return fmt.Errorf("PDF konnte nicht erstellt werden: %s", msgOr(msg, err))
	}
	return nil
}

func (s *Station) runOCR(ctx context.Context, pdf string) error {
	tmp := pdf + ".ocr"
	cmd := exec.CommandContext(ctx, "ocrmypdf",
		"-l", s.cfg.OCRLang, "--optimize", "1", "--quiet", pdf, tmp)
	if msg, err := runCaptured(cmd); err != nil {
		os.Remove(tmp)
		return errors.New(msgOr(msg, err))
	}
	return os.Rename(tmp, pdf)
}

func (s *Station) upload(ctx context.Context, pdf, remote string) error {
	cmd := exec.CommandContext(ctx, "rclone", "copy", pdf, strings.TrimSuffix(remote, "/")+"/")
	if msg, err := runCaptured(cmd); err != nil {
		return fmt.Errorf("Der Upload in die Cloud ist fehlgeschlagen: %s.", msgOr(msg, err))
	}
	return nil
}

// store legt das PDF im Ordner der Person ab, sichtbar nur fuer sie.
func (s *Station) store(pdf, userID, name string) error {
	dir := filepath.Join(s.cfg.DataDir, userID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("Zielordner konnte nicht angelegt werden: %w", err)
	}
	dst := filepath.Join(dir, name)

	// Kopieren statt Umbenennen, da /tmp auf einem anderen Dateisystem
	// liegen kann.
	in, err := os.Open(pdf)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

type StoredFile struct {
	Name    string
	Size    int64
	ModTime time.Time
}

func (s *Station) listFiles(userID string) ([]StoredFile, error) {
	dir := filepath.Join(s.cfg.DataDir, userID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var files []StoredFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pdf") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, StoredFile{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].ModTime.After(files[j].ModTime)
	})
	return files, nil
}

func (s *Station) filePath(userID, name string) (string, error) {
	if !validFileName.MatchString(name) {
		return "", errors.New("ungueltiger Dateiname")
	}
	return filepath.Join(s.cfg.DataDir, userID, name), nil
}

func (s *Station) deleteFile(userID, name string) error {
	p, err := s.filePath(userID, name)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

// cleanup loescht Dateien, die aelter sind als die konfigurierte Aufbewahrung.
func (s *Station) cleanup() {
	if s.cfg.RetentionDays <= 0 {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -s.cfg.RetentionDays)

	for _, u := range s.cfg.Users {
		files, err := s.listFiles(u.ID)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.ModTime.Before(cutoff) {
				if err := s.deleteFile(u.ID, f.Name); err == nil {
					log.Printf("Aufbewahrungsfrist abgelaufen, geloescht: %s/%s", u.ID, f.Name)
				}
			}
		}
	}
}

func (s *Station) startCleanupLoop() {
	go func() {
		for {
			s.cleanup()
			time.Sleep(6 * time.Hour)
		}
	}()
}
