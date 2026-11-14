package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
)

const (
	sourceADF     = "Automatic Document Feeder"
	sourceFlatbed = "FlatBed"
)

// Die Werte stammen aus `scanimage --help -d brother2:net1;dev0`.
var allowedModes = map[string]string{
	"gray":  "True Gray",
	"color": "24bit Color",
	"bw":    "Black & White",
}

var allowedResolutions = map[int]bool{
	100: true, 150: true, 200: true, 300: true, 400: true, 600: true,
}

var (
	validID       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	validPrefix   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	validFileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,120}\.(pdf|png)$`)
)

func newToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// scanLinesAndCR trennt Zeilen an \n und an \r. scanimage schreibt den
// Fortschritt mit Wagenruecklauf, ohne Zeilenumbruch.
func scanLinesAndCR(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func runCaptured(cmd *exec.Cmd) (string, error) {
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func msgOr(msg string, err error) string {
	if msg != "" {
		return firstLine(msg)
	}
	if err != nil {
		return err.Error()
	}
	return "unbekannter Fehler"
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 200 {
			return line[:200] + "..."
		}
		return line
	}
	return ""
}

func urlEncode(s string) string {
	return url.QueryEscape(s)
}

func formatSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f kB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
