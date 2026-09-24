//go:build ignore

// gen_icons renders the Gopherdex logo, a module box in the Go blues, to
// every raster size the site links: favicon.ico, the Apple touch icon,
// the web app manifest icons and the link-preview image. It draws the same
// polygons as static/favicon.svg, with supersampling, and needs no
// dependencies. Run it with: go generate ./web
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
)

type pt struct{ x, y float64 }

type shape struct {
	poly []pt
	c    color.RGBA
}

func rgb(h uint32) color.RGBA { return color.RGBA{uint8(h >> 16), uint8(h >> 8), uint8(h), 255} }

var (
	lightBlue  = rgb(0x5DC9E2)
	gopherBlue = rgb(0x00ADD8)
	darkBlue   = rgb(0x007D9C)
	white      = rgb(0xFFFFFF)
	slate50    = rgb(0xF9F9F9) // 3% Slate over white, the site's muted surface
)

// shapes is the logo in a 32×32 box, as in favicon.svg. tapeW is the
// width of the white tape line; small sizes draw it thicker so it stays
// visible.
func shapes(tapeW float64) []shape {
	a, b := pt{9.75, 6}, pt{22.25, 13}
	dx, dy := b.x-a.x, b.y-a.y
	l := math.Hypot(dx, dy)
	nx, ny := -dy/l*tapeW/2, dx/l*tapeW/2
	return []shape{
		{[]pt{{16, 2.5}, {28.5, 9.5}, {16, 16.5}, {3.5, 9.5}}, lightBlue},
		{[]pt{{3.5, 9.5}, {16, 16.5}, {16, 30}, {3.5, 23}}, gopherBlue},
		{[]pt{{28.5, 9.5}, {16, 16.5}, {16, 30}, {28.5, 23}}, darkBlue},
		{[]pt{{a.x + nx, a.y + ny}, {b.x + nx, b.y + ny}, {b.x - nx, b.y - ny}, {a.x - nx, a.y - ny}}, white},
	}
}

func inside(p pt, poly []pt) bool {
	in := false
	for i, j := 0, len(poly)-1; i < len(poly); j, i = i, i+1 {
		a, b := poly[i], poly[j]
		if (a.y > p.y) != (b.y > p.y) && p.x < (b.x-a.x)*(p.y-a.y)/(b.y-a.y)+a.x {
			in = !in
		}
	}
	return in
}

// render draws a w×h image with the logo size×size at (ox, oy), on bg
// (transparent when nil).
func render(w, h int, ox, oy, size float64, bg *color.RGBA, tapeW float64) *image.NRGBA {
	// NRGBA holds straight (not premultiplied) color, which is what the
	// averaging below produces; RGBA would wrap semi-transparent edges
	// into stray colors.
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	const ss = 6
	sh := shapes(tapeW)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					px := float64(x) + (float64(sx)+0.5)/ss
					py := float64(y) + (float64(sy)+0.5)/ss
					q := pt{(px - ox) / size * 32, (py - oy) / size * 32}
					c := bg
					for i := range sh {
						if inside(q, sh[i].poly) {
							c = &sh[i].c
						}
					}
					if c != nil {
						r, g, b, a = r+float64(c.R), g+float64(c.G), b+float64(c.B), a+255
					}
				}
			}
			if a > 0 {
				k := a / 255
				img.SetNRGBA(x, y, color.NRGBA{uint8(r / k), uint8(g / k), uint8(b / k), uint8(a / (ss * ss))})
			}
		}
	}
	return img
}

// icon is a square icon with the logo inset by pad pixels.
func icon(size int, pad float64, bg *color.RGBA, tapeW float64) *image.NRGBA {
	return render(size, size, pad, pad, float64(size)-2*pad, bg, tapeW)
}

func encode(img image.Image) []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		log.Fatal(err)
	}
	return buf.Bytes()
}

func write(name string, data []byte) {
	if err := os.WriteFile(filepath.Join("static", name), data, 0o644); err != nil {
		log.Fatal(err)
	}
}

func main() {
	// favicon.ico: 16, 32 and 48 px PNGs in one file.
	sizes := []int{16, 32, 48}
	var pngs [][]byte
	for _, s := range sizes {
		tw := 1.6
		if s == 16 {
			tw = 2.4
		}
		pngs = append(pngs, encode(icon(s, float64(s)/64, nil, tw)))
	}
	var ico bytes.Buffer
	binary.Write(&ico, binary.LittleEndian, []uint16{0, 1, uint16(len(pngs))})
	offset := 6 + 16*len(pngs)
	for i, s := range sizes {
		binary.Write(&ico, binary.LittleEndian, []uint8{uint8(s), uint8(s), 0, 0})
		binary.Write(&ico, binary.LittleEndian, []uint16{1, 32})
		binary.Write(&ico, binary.LittleEndian, []uint32{uint32(len(pngs[i])), uint32(offset)})
		offset += len(pngs[i])
	}
	for _, p := range pngs {
		ico.Write(p)
	}
	write("favicon.ico", ico.Bytes())

	// iOS home screen: opaque, with room around the box.
	write("apple-touch-icon.png", encode(icon(180, 28, &white, 1.6)))
	// Android and installed web apps. Maskable icons keep the logo inside
	// the central safe zone (80%), so every mask shape shows all of it.
	write("icon-192.png", encode(icon(192, 38, &white, 1.6)))
	write("icon-512.png", encode(icon(512, 100, &white, 1.6)))
	// Link previews (Slack, X, iMessage…): the box on the site's muted
	// surface; the page title supplies the words.
	write("og-image.png", encode(render(1200, 630, 450, 165, 300, &slate50, 1.6)))
	// GitHub's repository social preview, at the size it asks for. Upload
	// it under Settings → General → Social preview.
	write("social-preview.png", encode(render(1280, 640, 480, 160, 320, &slate50, 1.6)))
}
