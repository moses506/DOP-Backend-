// Package pdf writes simple business documents (quotations, invoices, delivery
// notes) as PDF without outside libraries.
//
// It supports what those documents need: text in Helvetica and Helvetica-Bold
// (the standard PDF fonts every reader has, so nothing is embedded), filled and
// outlined rectangles, lines and polygons, word wrapping, and a light rotated
// watermark. Coordinates are in points from the top-left of an A4 page.
// Text is encoded as Windows-1252, which covers English and the symbols used
// in DOS documents (– — · × ‘ ’ “ ” • … ©); other characters print as "?".
package pdf

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"image/color"
	"image/jpeg"
	"math"
	"strings"
	"time"
)

const (
	A4Width  = 595.28
	A4Height = 841.89
)

type Font int

const (
	Regular Font = iota
	Bold
)

// Color is an RGB colour, each part 0–1.
type Color struct{ R, G, B float64 }

// Hex makes a colour from "#rrggbb".
func Hex(s string) Color {
	var r, g, b int
	_, _ = fmt.Sscanf(strings.TrimPrefix(s, "#"), "%02x%02x%02x", &r, &g, &b)
	return Color{float64(r) / 255, float64(g) / 255, float64(b) / 255}
}

type Document struct {
	Title   string
	Author  string
	Subject string
	pages   []*Page
	images  []jpegImage
}

type jpegImage struct {
	data []byte
	w, h int
}

// AddJPEG registers a JPEG image for use on any page (stored once however
// often it is drawn) and returns its name for Page.Image.
func (d *Document) AddJPEG(data []byte) (string, error) {
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("pdf: image: %w", err)
	}
	if cfg.ColorModel != color.YCbCrModel && cfg.ColorModel != color.RGBAModel && cfg.ColorModel != color.GrayModel {
		return "", fmt.Errorf("pdf: image: only RGB or grey JPEGs are supported")
	}
	d.images = append(d.images, jpegImage{data: data, w: cfg.Width, h: cfg.Height})
	return fmt.Sprintf("Im%d", len(d.images)), nil
}

type Page struct {
	buf bytes.Buffer
	gs  bool // uses the faint graphics state (watermark)
}

func New() *Document { return &Document{} }

// AddPage starts a new A4 page and returns it.
func (d *Document) AddPage() *Page {
	p := &Page{}
	d.pages = append(d.pages, p)
	return p
}

func (d *Document) PageCount() int { return len(d.pages) }

// Pages returns the pages so far, e.g. to add "Page 1 of 3" footers at the end.
func (d *Document) Pages() []*Page { return d.pages }

func y(top float64) float64 { return A4Height - top }

func num(f float64) string {
	s := fmt.Sprintf("%.2f", f)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" || s == "-0" {
		return "0"
	}
	return s
}

func (p *Page) fill(c Color)   { fmt.Fprintf(&p.buf, "%s %s %s rg\n", num(c.R), num(c.G), num(c.B)) }
func (p *Page) stroke(c Color) { fmt.Fprintf(&p.buf, "%s %s %s RG\n", num(c.R), num(c.G), num(c.B)) }

// Rect draws a rectangle with its top-left corner at (x, top). Pass nil for no fill or no outline.
func (p *Page) Rect(x, top, w, h float64, fill, outline *Color, lineWidth float64) {
	if fill == nil && outline == nil {
		return
	}
	p.buf.WriteString("q\n")
	if fill != nil {
		p.fill(*fill)
	}
	if outline != nil {
		p.stroke(*outline)
		fmt.Fprintf(&p.buf, "%s w\n", num(lineWidth))
	}
	fmt.Fprintf(&p.buf, "%s %s %s %s re ", num(x), num(y(top+h)), num(w), num(h))
	switch {
	case fill != nil && outline != nil:
		p.buf.WriteString("B\n")
	case fill != nil:
		p.buf.WriteString("f\n")
	default:
		p.buf.WriteString("S\n")
	}
	p.buf.WriteString("Q\n")
}

// Image draws a registered image with its top-left corner at (x, top).
func (p *Page) Image(name string, x, top, w, h float64) {
	fmt.Fprintf(&p.buf, "q\n%s 0 0 %s %s %s cm\n/%s Do\nQ\n", num(w), num(h), num(x), num(y(top+h)), name)
}

// Line draws a straight line.
func (p *Page) Line(x1, top1, x2, top2 float64, c Color, width float64) {
	p.buf.WriteString("q\n")
	p.stroke(c)
	fmt.Fprintf(&p.buf, "%s w %s %s m %s %s l S\nQ\n", num(width), num(x1), num(y(top1)), num(x2), num(y(top2)))
}

// Polygon fills a closed shape through the points (x, top pairs).
func (p *Page) Polygon(points [][2]float64, c Color) {
	if len(points) < 3 {
		return
	}
	p.buf.WriteString("q\n")
	p.fill(c)
	for i, pt := range points {
		op := "l"
		if i == 0 {
			op = "m"
		}
		fmt.Fprintf(&p.buf, "%s %s %s ", num(pt[0]), num(y(pt[1])), op)
	}
	p.buf.WriteString("h f\nQ\n")
}

// Text writes s with its baseline at (x, baseline).
func (p *Page) Text(x, baseline float64, f Font, size float64, c Color, s string) {
	if s == "" {
		return
	}
	p.buf.WriteString("q\nBT\n")
	p.fill(c)
	fmt.Fprintf(&p.buf, "/F%d %s Tf %s %s Td (%s) Tj\nET\nQ\n", int(f)+1, num(size), num(x), num(y(baseline)), escape(s))
}

// TextRight writes s so that it ends at x.
func (p *Page) TextRight(x, baseline float64, f Font, size float64, c Color, s string) {
	p.Text(x-Width(f, size, s), baseline, f, size, c, s)
}

// TextCenter writes s centred on x.
func (p *Page) TextCenter(x, baseline float64, f Font, size float64, c Color, s string) {
	p.Text(x-Width(f, size, s)/2, baseline, f, size, c, s)
}

// Watermark writes large faint text diagonally across the page.
func (p *Page) Watermark(s string, c Color) {
	p.gs = true
	size := 110.0
	w := Width(Bold, size, s)
	a := 35 * math.Pi / 180
	cx, cy := A4Width/2, A4Height/2
	x := cx - math.Cos(a)*w/2
	yy := cy - math.Sin(a)*w/2
	p.buf.WriteString("q\n/GS1 gs\nBT\n")
	p.fill(c)
	fmt.Fprintf(&p.buf, "/F2 %s Tf %s %s %s %s %s %s Tm (%s) Tj\nET\nQ\n", num(size),
		fmt.Sprintf("%.4f", math.Cos(a)), fmt.Sprintf("%.4f", math.Sin(a)), fmt.Sprintf("%.4f", -math.Sin(a)), fmt.Sprintf("%.4f", math.Cos(a)),
		num(x), num(yy), escape(s))
}

// Wrap breaks s into lines no wider than width. Existing line breaks are kept.
func Wrap(f Font, size float64, s string, width float64) []string {
	var out []string
	for _, para := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		line := ""
		for _, w := range words {
			try := w
			if line != "" {
				try = line + " " + w
			}
			if Width(f, size, try) <= width {
				line = try
				continue
			}
			if line != "" {
				out = append(out, line)
			}
			// A single word longer than the line is split by characters.
			for Width(f, size, w) > width {
				cut := len([]rune(w))
				for cut > 1 && Width(f, size, string([]rune(w)[:cut])) > width {
					cut--
				}
				out = append(out, string([]rune(w)[:cut]))
				w = string([]rune(w)[cut:])
			}
			line = w
		}
		out = append(out, line)
	}
	return out
}

// Width is the width of s in points.
func Width(f Font, size float64, s string) float64 {
	widths := helvetica
	if f == Bold {
		widths = helveticaBold
	}
	total := 0
	for _, b := range encode(s) {
		if w, ok := widths[b]; ok {
			total += w
		} else {
			total += 556
		}
	}
	return float64(total) * size / 1000
}

// Bytes renders the document.
func (d *Document) Bytes() ([]byte, error) {
	var out bytes.Buffer
	var offsets []int
	obj := func(body string) int {
		offsets = append(offsets, out.Len())
		n := len(offsets)
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", n, body)
		return n
	}
	stream := func(dict string, data []byte) int {
		var z bytes.Buffer
		w := zlib.NewWriter(&z)
		_, _ = w.Write(data)
		_ = w.Close()
		offsets = append(offsets, out.Len())
		n := len(offsets)
		fmt.Fprintf(&out, "%d 0 obj\n<< %s /Filter /FlateDecode /Length %d >>\nstream\n", n, dict, z.Len())
		out.Write(z.Bytes())
		out.WriteString("\nendstream\nendobj\n")
		return n
	}

	out.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	// Object 1 is the catalog. Object 2, the page tree, is written last (once
	// the pages exist); its slot is reserved here.
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	offsets = append(offsets, 0)

	f1 := obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")
	f2 := obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold /Encoding /WinAnsiEncoding >>")
	gs := obj("<< /Type /ExtGState /ca 0.07 /CA 0.07 >>")
	var xobjects []string
	for i, im := range d.images {
		offsets = append(offsets, out.Len())
		n := len(offsets)
		cs := "/DeviceRGB"
		if c, _ := jpeg.DecodeConfig(bytes.NewReader(im.data)); c.ColorModel == color.GrayModel {
			cs = "/DeviceGray"
		}
		fmt.Fprintf(&out, "%d 0 obj\n<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace %s /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n",
			n, im.w, im.h, cs, len(im.data))
		out.Write(im.data)
		out.WriteString("\nendstream\nendobj\n")
		xobjects = append(xobjects, fmt.Sprintf("/Im%d %d 0 R", i+1, n))
	}
	res := obj(fmt.Sprintf("<< /Font << /F1 %d 0 R /F2 %d 0 R >> /ExtGState << /GS1 %d 0 R >> /XObject << %s >> >>",
		f1, f2, gs, strings.Join(xobjects, " ")))

	var kids []string
	for _, p := range d.pages {
		content := stream("", p.buf.Bytes())
		pg := obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %s %s] /Resources %d 0 R /Contents %d 0 R >>",
			num(A4Width), num(A4Height), res, content))
		kids = append(kids, fmt.Sprintf("%d 0 R", pg))
	}
	info := obj(fmt.Sprintf("<< /Title (%s) /Author (%s) /Subject (%s) /Producer (DOS BPA) /CreationDate (D:%s) >>",
		escape(d.Title), escape(d.Author), escape(d.Subject), time.Now().UTC().Format("20060102150405")+"Z"))

	// The page tree goes at the end; point object 2's offset at it.
	offsets[1] = out.Len()
	fmt.Fprintf(&out, "2 0 obj\n<< /Type /Pages /Kids [%s] /Count %d >>\nendobj\n", strings.Join(kids, " "), len(kids))

	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, o := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R /Info %d 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, info, xref)
	return out.Bytes(), nil
}

// ---- text encoding ----

var winAnsi = map[rune]byte{
	'€': 0x80, '‚': 0x82, '„': 0x84, '…': 0x85, '‘': 0x91, '’': 0x92, '“': 0x93, '”': 0x94,
	'•': 0x95, '–': 0x96, '—': 0x97, '™': 0x99, '©': 0xA9, '®': 0xAE, '°': 0xB0, '±': 0xB1,
	'·': 0xB7, '×': 0xD7, '÷': 0xF7, ' ': 0x20,
}

func encode(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r == '\t':
			out = append(out, ' ')
		case r >= 0x20 && r < 0x7f:
			out = append(out, byte(r))
		case r >= 0xa0 && r <= 0xff:
			out = append(out, byte(r))
		default:
			if b, ok := winAnsi[r]; ok {
				out = append(out, b)
			} else if r >= 0x20 {
				out = append(out, '?')
			}
		}
	}
	return out
}

func escape(s string) string {
	var b strings.Builder
	for _, c := range encode(s) {
		switch c {
		case '\\', '(', ')':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
