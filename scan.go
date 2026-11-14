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
	Output     string // outputPDF oder outputPNG
	Duplex     bool   // Einzug doppelseitig: zweiter Durchgang fuer Rueckseiten
	OCR        bool   // Texterkennung, Aufrichten und Geradeziehen (nur PDF)
	DropBlank  bool   // leere Seiten entfernen
}

const (
	outputPDF = "pdf" // alle Seiten in einer PDF-Datei
	outputPNG = "png" // jede Seite als eigene PNG-Datei
)

// outFile ist eine fertige Datei im Arbeitsverzeichnis samt Zielnamen.
type outFile struct {
	path string
	name string
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
	Waiting  bool      `json:"waiting"` // wartet auf das Umdrehen des Stapels
	Fronts   int       `json:"fronts"`  // Anzahl Vorderseiten beim Doppelseitig-Scan

	// Von der Person selbst abgebrochen: dann keine Fehlermeldung schicken.
	canceled bool

	// Arbeitsverzeichnis mit den bisher gescannten Seiten, nur waehrend
	// der Auftrag laeuft. Fuer die Vorschau.
	workdir string
}

// Station fuehrt die Scanauftraege aus. Das Geraet kann nur einen Auftrag
// gleichzeitig bedienen, deshalb laeuft hier immer hoechstens einer.
type Station struct {
	cfg Config

	mu     sync.Mutex
	job    *Job
	busy   bool
	cancel context.CancelFunc

	previewMu sync.Mutex
	flip      chan duplexAction

	notify    *Notifier  // nil, wenn Matrix nicht eingerichtet ist
	paperless *Paperless // nil, wenn Paperless nicht eingerichtet ist
}

func NewStation(cfg Config) *Station {
	return &Station{cfg: cfg, notify: NewNotifier(cfg), paperless: NewPaperless(cfg)}
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
	s.flip = make(chan duplexAction, 1)
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
		s.flip = nil
		job.Waiting = false

		if !job.canceled {
			s.notify.Notify(u.ID, resultMessage(job, opts)+s.notify.link())
		}
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
		s.job.canceled = true
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
	s.mu.Lock()
	job.workdir = workdir
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		job.workdir = ""
		s.mu.Unlock()
		os.RemoveAll(workdir)
	}()

	label := "Seite"
	if opts.Duplex {
		label = "Vorderseite"
	}
	pages, err := s.scanPages(ctx, job, workdir, opts, 1, label)
	if err != nil {
		return err
	}
	s.setPages(job, len(pages))

	var notes []string
	if opts.Duplex {
		pages, notes, err = s.scanBacks(ctx, job, workdir, opts, pages)
		if err != nil {
			return err
		}
	}

	if opts.DropBlank {
		s.setStep(job, "Leere Seiten suchen", 0)
		var removed int
		pages, removed = dropBlank(pages, opts.Resolution)
		if removed > 0 {
			notes = append(notes, fmt.Sprintf("%d leere Seite(n) entfernt.", removed))
		}
	}
	s.setPages(job, len(pages))
	if len(notes) > 0 {
		s.mu.Lock()
		job.Note = strings.Join(notes, " ")
		s.mu.Unlock()
	}

	base := fmt.Sprintf("%s_%s", opts.Prefix, job.Started.Format("2006-01-02_1504"))

	var outs []outFile
	if opts.Output == outputPNG {
		// Einzelne Seiten: kein PDF, keine Texterkennung.
		for i, p := range pages {
			outs = append(outs, outFile{path: p, name: fmt.Sprintf("%s_s%03d.png", base, i+1)})
		}
	} else {
		name := base + ".pdf"
		pdf := filepath.Join(workdir, name)

		s.setStep(job, "PDF erstellen", 0)
		if err := s.makePDF(ctx, pages, pdf); err != nil {
			return err
		}

		// Texterkennung nur fuer die Download-Ablage. Ziele wie Paperless
		// erkennen den Text selbst und drehen dabei auch die Seiten; eine
		// eigene Textebene wuerde das dort verhindern (OCR-Modus "skip").
		if opts.OCR && ocrAvailable() && u.Target == "download" {
			s.setStep(job, "Texterkennung", 0)
			if err := s.runOCR(ctx, pdf); err != nil {
				// Ohne Texterkennung ist das PDF weiterhin brauchbar.
				log.Printf("Auftrag %s: OCR uebersprungen: %v", job.ID, err)
				s.mu.Lock()
				job.Note = strings.TrimSpace(job.Note + " Die Texterkennung ist fehlgeschlagen, das PDF enthält nur Bilder.")
				s.mu.Unlock()
			}
		}
		outs = []outFile{{path: pdf, name: name}}
	}

	// Immer zuerst lokal ablegen. Bei Paperless ist das die Sicherungskopie,
	// falls Paperless den Scan ablehnt oder nicht erreichbar ist; sie wird
	// mit der Aufbewahrungsfrist geloescht.
	s.setStep(job, "Ablegen", 0)
	if err := s.storeAll(outs, u.ID); err != nil {
		return err
	}
	s.mu.Lock()
	job.File = outs[0].name
	s.mu.Unlock()

	if u.Target == "paperless" {
		if s.paperless == nil {
			return errors.New("Paperless ist nicht eingerichtet. Der Scan liegt hier zum Download bereit.")
		}
		s.setStep(job, "An Paperless senden", 0)
		var tasks []paperlessTask
		for _, o := range outs {
			task, err := s.paperless.Upload(ctx, o.path, o.name)
			if err != nil {
				return fmt.Errorf("%v Der Scan liegt hier zum Download bereit.", err)
			}
			tasks = append(tasks, paperlessTask{id: task, name: o.name})
		}
		s.mu.Lock()
		job.Note = strings.TrimSpace(job.Note + " An Paperless übergeben, die Verarbeitung dort dauert einen Moment.")
		s.mu.Unlock()
		go s.followPaperless(u.ID, tasks)
	}

	return nil
}

func (s *Station) storeAll(outs []outFile, userID string) error {
	for _, o := range outs {
		if err := s.store(o.path, userID, o.name); err != nil {
			return err
		}
	}
	return nil
}

// scanBacks wartet, bis der Stapel umgedreht ist, scannt die Rueckseiten und
// setzt alles in die richtige Reihenfolge.
func (s *Station) scanBacks(ctx context.Context, job *Job, workdir string, opts ScanOptions, fronts []string) ([]string, []string, error) {
	s.mu.Lock()
	job.Waiting = true
	job.Fronts = len(fronts)
	job.Step = "Stapel umdrehen und erneut einlegen"
	job.Progress = 0
	flip := s.flip
	s.mu.Unlock()

	s.notify.Notify(job.UserID, fmt.Sprintf(
		"🔄 Vorderseiten fertig (%d). Stapel aus der Ablage nehmen, als Ganzes umdrehen "+
			"und wieder einlegen. Dann hier „weiter“ antworten (oder „fertig“ ohne Rückseiten).",
		len(fronts))+s.notify.link())

	var act duplexAction
	select {
	case act = <-flip:
	case <-ctx.Done():
		return nil, nil, errors.New("Der Scan wurde abgebrochen oder hat zu lange gedauert.")
	case <-time.After(duplexWait):
		s.mu.Lock()
		job.Waiting = false
		s.mu.Unlock()
		return fronts, []string{"Es wurde nicht umgedreht, nur die Vorderseiten wurden gespeichert."}, nil
	}
	if act == duplexSkip {
		return fronts, nil, nil
	}

	backs, err := s.scanPages(ctx, job, workdir, opts, len(fronts)+1, "Rückseite")
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, err
		}
		// Die Vorderseiten nicht verlieren, nur weil der zweite Durchgang
		// scheiterte.
		return fronts, []string{"Die Rückseiten konnten nicht gescannt werden (" + err.Error() + "), gespeichert sind nur die Vorderseiten."}, nil
	}

	all, ok := interleave(fronts, backs)
	if !ok {
		return all, []string{fmt.Sprintf(
			"Achtung: %d Vorderseiten, aber %d Rückseiten. Die Reihenfolge stimmt ab der Abweichung vermutlich nicht.",
			len(fronts), len(backs))}, nil
	}
	return all, nil, nil
}

// resultMessage formuliert die Nachricht zum Ende eines Auftrags.
func resultMessage(job *Job, opts ScanOptions) string {
	if job.State == StateError {
		return "❌ Scan fehlgeschlagen: " + job.Note
	}
	what := fmt.Sprintf("%d Seite(n)", job.Pages)
	if opts.Output == outputPNG {
		what = fmt.Sprintf("%d Bild(er)", job.Pages)
	}
	file := job.File
	if opts.Output == outputPNG && job.Pages > 1 {
		// ..._s001.png bis ..._s004.png
		file = fmt.Sprintf("%s bis %s", job.File,
			strings.Replace(job.File, "_s001.png", fmt.Sprintf("_s%03d.png", job.Pages), 1))
	}
	msg := fmt.Sprintf("✅ Scan fertig: %s, %s", what, file)
	if job.Note != "" {
		msg += "\n" + job.Note
	}
	return msg
}

func ocrAvailable() bool {
	_, err := exec.LookPath("ocrmypdf")
	return err == nil
}

var progressRe = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)%`)

// scanPages scannt einen Durchgang. Die Seiten werden ab Nummer start
// abgelegt (page_<start>.png, ...), zurueck kommen nur die dieses Durchgangs.
// label beschriftet den Fortschritt ("Seite", "Vorderseite", "Rueckseite").
func (s *Station) scanPages(ctx context.Context, job *Job, workdir string, opts ScanOptions, start int, label string) ([]string, error) {
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
		args = append(args, "-o", filepath.Join(workdir, fmt.Sprintf("page_%03d.png", start)))
	} else {
		args = append(args, "--batch="+filepath.Join(workdir, "page_%03d.png"),
			fmt.Sprintf("--batch-start=%d", start), "--batch-print")
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
			// "Scanning page N" kommt zu Beginn der Seite, pagesSeen ist also
			// schon die aktuelle Seite. Beim Flachbett fehlt die Zeile.
			s.setStep(job, fmt.Sprintf("%s %d scannen", label, max(pagesSeen, 1)), int(p))
			continue
		}
		if strings.Contains(line, "Scanning page") {
			pagesSeen++
			s.setStep(job, fmt.Sprintf("%s %d scannen", label, pagesSeen), 0)
			s.setPages(job, start-1+pagesSeen)
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
		var n int
		if _, err := fmt.Sscanf(filepath.Base(p), "page_%d.png", &n); err != nil || n < start {
			continue // anderer Durchgang
		}
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
		// Kommt auch, wenn der Treiber das Gerät gar nicht erreicht oder das
		// SANE-Backend nicht geladen wurde, nicht nur bei falschen Optionen.
		return "Das Gerät hat die Einstellungen abgelehnt oder ist nicht erreichbar. " +
			"Prüfen Sie, ob der Scanner eingeschaltet ist, und versuchen Sie sonst eine niedrigere Auflösung."
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
		"-l", s.cfg.OCRLang, "--optimize", "1",
		// Verkehrt herum oder quer eingelegte Seiten aufrichten (Tesseract
		// erkennt die Ausrichtung). Sonst wuerde der Text auf dem Kopf
		// gelesen, und Paperless dreht Seiten mit Textebene nicht mehr.
		// Die Erkennung ist bei diesen Scans recht unsicher (Konfidenz oft
		// 3-12), die Standardschwelle 14 dreht fast nichts. Getestet: Seiten,
		// die schon richtig stehen, werden zuverlaessig als aufrecht erkannt
		// und nicht gedreht, deshalb ist eine niedrige Schwelle unbedenklich.
		"--rotate-pages", "--rotate-pages-threshold", "2",
		// Schraeg eingezogene Blaetter gerade ziehen (gemessen 0,5 bis 2,5
		// Grad, je Blatt verschieden).
		"--deskew",
		"--quiet", pdf, tmp)
	if msg, err := runCaptured(cmd); err != nil {
		os.Remove(tmp)
		return errors.New(msgOr(msg, err))
	}
	return os.Rename(tmp, pdf)
}

type paperlessTask struct {
	id   string
	name string
}

// followPaperless verfolgt im Hintergrund, ob Paperless die Dokumente
// angenommen hat, und meldet das Ergebnis per Matrix.
func (s *Station) followPaperless(userID string, tasks []paperlessTask) {
	for _, t := range tasks {
		ctx, cancel := context.WithTimeout(context.Background(), paperlessFollow)
		res, err := s.paperless.Follow(ctx, t.id)
		cancel()
		switch {
		case err != nil:
			log.Printf("Paperless: Ergebnis fuer %s unbekannt: %v", t.name, err)
		case res.OK:
			log.Printf("Paperless: %s angenommen %s", t.name, res.DocURL)
			msg := "📄 In Paperless angekommen: " + t.name
			if res.DocURL != "" {
				msg += "\n" + res.DocURL
			}
			s.notify.Notify(userID, msg)
		default:
			log.Printf("Paperless: %s abgelehnt: %s", t.name, res.Message)
			s.notify.Notify(userID, fmt.Sprintf(
				"⚠️ Paperless hat %s nicht übernommen: %s\nDie Datei liegt noch zum Download bereit.",
				t.name, res.Message)+s.notify.link())
		}
	}
}

// store legt eine Datei im Ordner der Person ab, sichtbar nur fuer sie.
func (s *Station) store(src, userID, name string) error {
	dir := filepath.Join(s.cfg.DataDir, userID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("Zielordner konnte nicht angelegt werden: %w", err)
	}
	dst := filepath.Join(dir, name)

	// Kopieren statt Umbenennen, da /tmp auf einem anderen Dateisystem
	// liegen kann.
	in, err := os.Open(src)
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
		if e.IsDir() || !validFileName.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, StoredFile{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool {
		// Seiten eines Auftrags haben fast dieselbe Zeit, deshalb pro
		// Minute nach Namen sortieren, damit s001, s002, ... in Reihe stehen.
		ti, tj := files[i].ModTime.Truncate(time.Minute), files[j].ModTime.Truncate(time.Minute)
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return files[i].Name < files[j].Name
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
