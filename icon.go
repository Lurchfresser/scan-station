package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
)

// Symbol der Station: ein weisses Blatt mit Textzeilen auf dem gruenen
// Akzentton, quer darueber der helle Balken des Scanners. Als SVG fuer den
// Browser-Tab, als PNG fuer den Startbildschirm von Handys (die dort kein
// SVG nehmen). Beide werden aus denselben Formen erzeugt.

type iconShape struct {
	x, y, w, h, r float64 // im Raster 64 x 64
	c             color.RGBA
}

var (
	iconGreen = color.RGBA{0x2f, 0x6f, 0x4f, 0xff}
	iconPaper = color.RGBA{0xff, 0xff, 0xff, 0xff}
	iconLine  = color.RGBA{0xa9, 0xc4, 0xb5, 0xff}
	iconBeam  = color.RGBA{0x7c, 0xe3, 0xa6, 0xff}
)

var iconShapes = []iconShape{
	{0, 0, 64, 64, 14, iconGreen},  // Hintergrund
	{17, 9, 30, 42, 3, iconPaper},  // Blatt
	{22, 16, 20, 3, 1.5, iconLine}, // Textzeilen
	{22, 22, 20, 3, 1.5, iconLine},
	{22, 28, 14, 3, 1.5, iconLine},
	{22, 44, 16, 3, 1.5, iconLine},
	{9, 34, 46, 6, 3, iconBeam}, // Scanbalken
}

func iconSVG() []byte {
	var b bytes.Buffer
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64">`)
	for _, s := range iconShapes {
		b.WriteString(`<rect x="` + ftoa(s.x) + `" y="` + ftoa(s.y) + `" width="` + ftoa(s.w) +
			`" height="` + ftoa(s.h) + `" rx="` + ftoa(s.r) + `" fill="` + hexColor(s.c) + `"/>`)
	}
	b.WriteString(`</svg>`)
	return b.Bytes()
}

// iconPNG zeichnet das Symbol in size x size Pixeln, mit 4x4-Unterabtastung
// fuer glatte Kanten.
func iconPNG(size int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	scale := 64.0 / float64(size)
	const ss = 4
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var r, g, bl, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/ss) * scale
					y := (float64(py) + (float64(sy)+0.5)/ss) * scale
					// Oberste Form, die den Punkt enthaelt, gewinnt.
					for i := len(iconShapes) - 1; i >= 0; i-- {
						if s := iconShapes[i]; inRoundRect(x, y, s) {
							r += float64(s.c.R)
							g += float64(s.c.G)
							bl += float64(s.c.B)
							a += 255
							break
						}
					}
				}
			}
			n := float64(ss * ss)
			if a == 0 {
				continue
			}
			// Vormultipliziert, wie image.RGBA es erwartet.
			img.SetRGBA(px, py, color.RGBA{uint8(r / n), uint8(g / n), uint8(bl / n), uint8(a / n)})
		}
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

func inRoundRect(x, y float64, s iconShape) bool {
	if x < s.x || y < s.y || x > s.x+s.w || y > s.y+s.h {
		return false
	}
	// Ecken: Abstand zum Mittelpunkt des Eckkreises pruefen.
	cx := clamp(x, s.x+s.r, s.x+s.w-s.r)
	cy := clamp(y, s.y+s.r, s.y+s.h-s.r)
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= s.r*s.r
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func ftoa(f float64) string {
	if f == float64(int(f)) {
		return itoa(int(f))
	}
	return itoa(int(f)) + "." + itoa(int((f-float64(int(f)))*10+0.5))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for ; n > 0; n /= 10 {
		d = append([]byte{byte('0' + n%10)}, d...)
	}
	return string(d)
}

func hexColor(c color.RGBA) string {
	const hx = "0123456789abcdef"
	return string([]byte{'#', hx[c.R>>4], hx[c.R&15], hx[c.G>>4], hx[c.G&15], hx[c.B>>4], hx[c.B&15]})
}

// Einmal beim Start erzeugt.
var (
	faviconSVG     = iconSVG()
	faviconPNG32   = iconPNG(32)
	touchIconPNG   = iconPNG(180)
	iconCacheValue = "public, max-age=604800"
)

// handleIcon liefert die Symbole aus, ohne Anmeldung.
func handleIcon(data []byte, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", iconCacheValue)
		w.Write(data)
	}
}
