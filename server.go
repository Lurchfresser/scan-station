package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	cookieName     = "scanstation"
	sessionTTL     = 12 * time.Hour
	maxLoginTries  = 6
	loginLockout   = 10 * time.Minute
	cleanupSession = time.Hour
)

type session struct {
	userID  string
	expires time.Time
}

type loginAttempt struct {
	count int
	until time.Time
}

type Server struct {
	cfg Config
	st  *Station

	mu       sync.Mutex
	sessions map[string]session
	attempts map[string]*loginAttempt
}

func NewServer(cfg Config, st *Station) *Server {
	s := &Server{
		cfg:      cfg,
		st:       st,
		sessions: map[string]session{},
		attempts: map[string]*loginAttempt{},
	}
	go s.sessionJanitor()
	return s
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)
	mux.HandleFunc("/scan", s.handleScan)
	mux.HandleFunc("/cancel", s.handleCancel)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/download/", s.handleDownload)
	mux.HandleFunc("/delete", s.handleDelete)
	return mux
}

// ---------------------------------------------------------------- Sitzungen

func (s *Server) sessionJanitor() {
	for {
		time.Sleep(cleanupSession)
		now := time.Now()
		s.mu.Lock()
		for tok, sess := range s.sessions {
			if now.After(sess.expires) {
				delete(s.sessions, tok)
			}
		}
		for ip, a := range s.attempts {
			if now.After(a.until) {
				delete(s.attempts, ip)
			}
		}
		s.mu.Unlock()
	}
}

func (s *Server) currentUser(r *http.Request) (User, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return User{}, false
	}

	s.mu.Lock()
	sess, ok := s.sessions[c.Value]
	if ok && time.Now().After(sess.expires) {
		delete(s.sessions, c.Value)
		ok = false
	}
	s.mu.Unlock()

	if !ok {
		return User{}, false
	}
	return s.cfg.userByID(sess.userID)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// checkLockout meldet, ob von dieser Adresse gerade Anmeldeversuche
// gesperrt sind, und wie lange noch.
func (s *Server) checkLockout(ip string) (bool, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.attempts[ip]
	if !ok {
		return false, 0
	}
	if a.count >= maxLoginTries && time.Now().Before(a.until) {
		return true, time.Until(a.until).Round(time.Second)
	}
	if time.Now().After(a.until) {
		delete(s.attempts, ip)
	}
	return false, 0
}

func (s *Server) noteFailedLogin(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.attempts[ip]
	if !ok {
		a = &loginAttempt{}
		s.attempts[ip] = a
	}
	a.count++
	a.until = time.Now().Add(loginLockout)
}

func (s *Server) clearFailedLogins(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.attempts, ip)
}

// ---------------------------------------------------------------- Handler

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	u, ok := s.currentUser(r)
	if !ok {
		s.renderLogin(w, r, "")
		return
	}
	s.renderMain(w, r, u, r.URL.Query().Get("err"), r.URL.Query().Get("info"))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	ip := clientIP(r)
	if locked, wait := s.checkLockout(ip); locked {
		s.renderLogin(w, r, fmt.Sprintf("Zu viele Fehlversuche. Bitte in %s erneut probieren.", wait))
		return
	}

	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, r, "Die Eingabe konnte nicht gelesen werden.")
		return
	}

	id := r.FormValue("user")
	pin := r.FormValue("pin")

	u, found := s.cfg.userByID(id)
	// Der PIN-Vergleich laeuft in konstanter Zeit, und bei unbekanntem
	// Benutzer wird gegen einen Platzhalter verglichen, damit die Antwortzeit
	// nichts verraet.
	expected := "kein-benutzer-vorhanden"
	if found {
		expected = u.PIN
	}
	okPIN := subtle.ConstantTimeCompare([]byte(pin), []byte(expected)) == 1

	if !found || !okPIN {
		s.noteFailedLogin(ip)
		log.Printf("Fehlgeschlagene Anmeldung von %s (Benutzer %q)", ip, id)
		s.renderLogin(w, r, "Name oder PIN stimmt nicht.")
		return
	}

	s.clearFailedLogins(ip)

	tok := newToken(24)
	s.mu.Lock()
	s.sessions[tok] = session{userID: u.ID, expires: time.Now().Add(sessionTTL)}
	s.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		redirectErr(w, r, "Die Eingabe konnte nicht gelesen werden.")
		return
	}

	mode, ok := allowedModes[r.FormValue("mode")]
	if !ok {
		mode = allowedModes["gray"]
	}

	res, err := strconv.Atoi(r.FormValue("resolution"))
	if err != nil || !allowedResolutions[res] {
		res = 200
	}

	source := sourceADF
	if r.FormValue("source") == "flatbed" {
		source = sourceFlatbed
	}

	prefix := strings.TrimSpace(r.FormValue("prefix"))
	if prefix == "" {
		prefix = "scan"
	}
	if !validPrefix.MatchString(prefix) {
		redirectErr(w, r, "Der Name darf nur Buchstaben, Ziffern, Bindestrich und Unterstrich enthalten (max. 32 Zeichen).")
		return
	}

	opts := ScanOptions{Mode: mode, Resolution: res, Source: source, Prefix: prefix}
	if err := s.st.Start(u, opts); err != nil {
		redirectErr(w, r, "Es läuft gerade ein anderer Scan. Bitte warten, das Gerät kann nur einen Auftrag auf einmal.")
		return
	}
	log.Printf("Auftrag gestartet von %s: %s, %d dpi, %s", u.ID, mode, res, source)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok || r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	job := s.st.Snapshot()
	// Nur der eigene Auftrag darf abgebrochen werden.
	if job.State == StateRunning && job.UserID != u.ID {
		redirectErr(w, r, "Dieser Scan gehört jemand anderem.")
		return
	}
	s.st.Cancel()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

type statusResponse struct {
	State    JobState `json:"state"`
	Step     string   `json:"step"`
	Progress int      `json:"progress"`
	Pages    int      `json:"pages"`
	Note     string   `json:"note"`
	File     string   `json:"file"`
	Mine     bool     `json:"mine"`
	Who      string   `json:"who"`
	Elapsed  int      `json:"elapsed"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		http.Error(w, "nicht angemeldet", http.StatusUnauthorized)
		return
	}

	job := s.st.Snapshot()
	resp := statusResponse{
		State:    job.State,
		Step:     job.Step,
		Progress: job.Progress,
		Pages:    job.Pages,
		Mine:     job.UserID == u.ID,
		Who:      job.UserName,
	}
	if resp.Mine {
		resp.Note = job.Note
		resp.File = job.File
	}
	if !job.Started.IsZero() {
		end := job.Finished
		if job.State == StateRunning {
			end = time.Now()
		}
		resp.Elapsed = int(end.Sub(job.Started).Seconds())
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	name := strings.TrimPrefix(r.URL.Path, "/download/")
	path, err := s.st.filePath(u.ID, name)
	if err != nil {
		http.Error(w, "ungültiger Dateiname", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	http.ServeFile(w, r, path)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok || r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		redirectErr(w, r, "Die Eingabe konnte nicht gelesen werden.")
		return
	}

	name := r.FormValue("name")
	if err := s.st.deleteFile(u.ID, name); err != nil {
		redirectErr(w, r, "Die Datei konnte nicht gelöscht werden.")
		return
	}
	http.Redirect(w, r, "/?info="+urlEncode("Datei gelöscht."), http.StatusSeeOther)
}

func redirectErr(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/?err="+urlEncode(msg), http.StatusSeeOther)
}
