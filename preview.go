package main

import (
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
)

// Breite der Vorschaubilder in Pixeln. Reicht, um auf dem Handy zu sehen,
// ob die Seite richtig herum und vollstaendig eingezogen wurde.
const previewWidth = 480

var errNoPreview = errors.New("keine Vorschau vorhanden")

// pageFile liefert den Pfad der n-ten fertig gescannten Seite des laufenden
// Auftrags, sofern er der angegebenen Person gehoert. scanimage schreibt
// jede Seite zuerst als .part und benennt sie erst am Ende um, eine Datei
// ohne Endung .part ist also vollstaendig.
func (s *Station) pageFile(userID string, n int) (dir, page string, err error) {
	if n < 1 || n > 999 {
		return "", "", errNoPreview
	}
	s.mu.Lock()
	job := s.job
	ok := job != nil && s.busy && job.UserID == userID && job.workdir != ""
	if ok {
		dir = job.workdir
	}
	s.mu.Unlock()
	if !ok {
		return "", "", errNoPreview
	}
	page = filepath.Join(dir, fmt.Sprintf("page_%03d.png", n))
	if st, err := os.Stat(page); err != nil || st.Size() == 0 {
		return "", "", errNoPreview
	}
	return dir, page, nil
}

// CompletedPages zaehlt die fertigen Seiten des laufenden Auftrags.
func (s *Station) CompletedPages(userID string) int {
	n := 0
	for {
		if _, _, err := s.pageFile(userID, n+1); err != nil {
			return n
		}
		n++
	}
}

// Preview gibt den Pfad eines verkleinerten JPEG der n-ten Seite zurueck und
// erzeugt es beim ersten Aufruf.
func (s *Station) Preview(userID string, n int) (string, error) {
	dir, page, err := s.pageFile(userID, n)
	if err != nil {
		return "", err
	}
	thumb := filepath.Join(dir, fmt.Sprintf("thumb_%03d.jpg", n))

	// Nur eine Vorschau auf einmal erzeugen, das Dekodieren grosser PNGs
	// kostet Speicher.
	s.previewMu.Lock()
	defer s.previewMu.Unlock()

	if _, err := os.Stat(thumb); err == nil {
		return thumb, nil
	}
	if err := makeThumbnail(page, thumb, previewWidth); err != nil {
		return "", err
	}
	return thumb, nil
}

func makeThumbnail(src, dst string, maxW int) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	img, err := png.Decode(in)
	in.Close()
	if err != nil {
		return err
	}

	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := jpeg.Encode(out, downscale(img, maxW), &jpeg.Options{Quality: 75}); err != nil {
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

// downscale verkleinert auf maxW Pixel Breite. Jedes Zielpixel ist der
// Mittelwert einiger Stichproben aus seinem Quellbereich; das ist fuer eine
// Vorschau scharf genug und bleibt auch bei 600 dpi schnell.
func downscale(src image.Image, maxW int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxW || w == 0 || h == 0 {
		return src
	}
	tw := maxW
	th := h * tw / w
	if th < 1 {
		th = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))

	const samples = 3
	for y := 0; y < th; y++ {
		y0, y1 := y*h/th, (y+1)*h/th
		for x := 0; x < tw; x++ {
			x0, x1 := x*w/tw, (x+1)*w/tw
			var r, g, bl, cnt uint32
			for sy := 0; sy < samples; sy++ {
				py := y0 + (y1-y0)*sy/samples
				for sx := 0; sx < samples; sx++ {
					px := x0 + (x1-x0)*sx/samples
					cr, cg, cb, _ := src.At(b.Min.X+px, b.Min.Y+py).RGBA()
					r += cr >> 8
					g += cg >> 8
					bl += cb >> 8
					cnt++
				}
			}
			i := dst.PixOffset(x, y)
			dst.Pix[i+0] = uint8(r / cnt)
			dst.Pix[i+1] = uint8(g / cnt)
			dst.Pix[i+2] = uint8(bl / cnt)
			dst.Pix[i+3] = 0xff
		}
	}
	return dst
}
