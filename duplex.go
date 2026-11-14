package main

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"time"
)

// Doppelseitiges Scannen mit einem Einzug, der nur eine Seite kann:
// Erst laufen alle Vorderseiten durch, dann dreht man den Stapel um und
// legt ihn erneut ein. Die Rueckseiten kommen dabei in umgekehrter
// Reihenfolge (die Rueckseite des letzten Blatts zuerst).

type duplexAction int

const (
	duplexContinue duplexAction = iota // Rueckseiten scannen
	duplexSkip                         // ohne Rueckseiten fertigstellen
)

// So lange wartet der Auftrag auf das Umdrehen, danach wird er mit den
// Vorderseiten abgeschlossen, damit das Geraet nicht ewig blockiert ist.
const duplexWait = 30 * time.Minute

// DuplexAction meldet dem wartenden Auftrag, wie es weitergeht. Nur die
// Person, der der Auftrag gehoert, darf das.
func (s *Station) DuplexAction(userID string, a duplexAction) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.busy || s.job == nil || s.job.UserID != userID || !s.job.Waiting || s.flip == nil {
		return false
	}
	select {
	case s.flip <- a:
		s.job.Waiting = false
		return true
	default:
		return false
	}
}

// interleave setzt Vorder- und Rueckseiten zusammen. backs liegt in
// Scanreihenfolge vor, also umgekehrt. Stimmen die Anzahlen nicht ueberein
// (Doppeleinzug, Blatt vergessen), wird so weit wie moeglich verschraenkt
// und der Rest angehaengt; ok ist dann false.
func interleave(fronts, backs []string) (out []string, ok bool) {
	n := len(fronts)
	if len(backs) < n {
		n = len(backs)
	}
	for i := 0; i < n; i++ {
		out = append(out, fronts[i], backs[len(backs)-1-i])
	}
	out = append(out, fronts[n:]...)
	for i := len(backs) - 1 - n; i >= 0; i-- {
		out = append(out, backs[i])
	}
	return out, len(fronts) == len(backs)
}

// Erkennung leerer Seiten: Die Seite wird in Zellen von 2 mm geteilt. Eine
// Zelle zaehlt als beschrieben, wenn mindestens 3 % ihrer Pixel dunkel sind.
// Schrift und Striche fuellen Zellen dicht, Staubkoerner und Flecken nur
// duenn. Ein kurzes Wort belegt schon mehrere Zellen, deshalb gilt eine
// Seite erst bei hoechstens blankMaxCells beschriebenen Zellen als leer.
const (
	blankCellMM     = 2.0
	blankCellInk    = 0.03
	blankMaxCells   = 2
	blankMarginPerc = 6 // Rand weglassen: Blattkante, Lochung, Einzugsspuren
)

// isBlank prueft, ob eine gescannte Seite praktisch leer ist. dpi ist die
// Scanaufloesung, daraus ergibt sich die Zellgroesse in Pixeln.
func isBlank(path string, dpi int) (bool, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, 0, err
	}
	img, err := png.Decode(f)
	f.Close()
	if err != nil {
		return false, 0, err
	}
	cells := inkCells(img, dpi)
	return cells <= blankMaxCells, cells, nil
}

func inkCells(img image.Image, dpi int) int {
	b := img.Bounds()
	mx, my := b.Dx()*blankMarginPerc/100, b.Dy()*blankMarginPerc/100
	r := image.Rect(b.Min.X+mx, b.Min.Y+my, b.Max.X-mx, b.Max.Y-my)

	cs := int(float64(dpi) * blankCellMM / 25.4)
	if cs < 4 {
		cs = 4
	}
	gray, isGray := img.(*image.Gray)
	dark := func(x, y int) bool {
		if isGray {
			return gray.Pix[gray.PixOffset(x, y)] < 128
		}
		return color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y < 128
	}

	need := int(float64(cs*cs) * blankCellInk)
	cells := 0
	for cy := r.Min.Y; cy+cs <= r.Max.Y; cy += cs {
		for cx := r.Min.X; cx+cs <= r.Max.X; cx += cs {
			n := 0
			for y := cy; y < cy+cs && n < need; y++ {
				for x := cx; x < cx+cs; x++ {
					if dark(x, y) {
						n++
					}
				}
			}
			if n >= need {
				cells++
			}
		}
	}
	return cells
}

// dropBlank entfernt leere Seiten, behaelt aber mindestens eine.
func dropBlank(pages []string, dpi int) (kept []string, removed int) {
	for _, p := range pages {
		if blank, _, err := isBlank(p, dpi); err == nil && blank {
			removed++
			continue
		}
		kept = append(kept, p)
	}
	if len(kept) == 0 {
		return pages, 0
	}
	return kept, removed
}
