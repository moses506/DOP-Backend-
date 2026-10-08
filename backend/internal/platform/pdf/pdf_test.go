package pdf

import (
	"os"
	"strings"
	"testing"
)

func TestDocumentRenders(t *testing.T) {
	d := New()
	d.Title = "Test (quote)"
	p := d.AddPage()
	navy := Hex("#141b47")
	p.Rect(40, 40, 200, 30, &navy, nil, 0)
	p.Text(48, 60, Bold, 14, Color{1, 1, 1}, "Quotation – DOS-Q-2026-0001 (Rev 2)")
	p.TextRight(555, 100, Regular, 10, navy, "ZMW 12,500.00")
	p.Line(40, 110, 555, 110, navy, 0.5)
	p.Polygon([][2]float64{{40, 130}, {60, 140}, {40, 150}}, navy)
	p.Watermark("DRAFT", navy)
	d.AddPage().Text(40, 60, Regular, 10, navy, "Page 2 · 4 × Canon “C3226i”")
	b, err := d.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "%PDF-1.4") || !strings.HasSuffix(string(b), "%%EOF\n") {
		t.Fatal("not a PDF")
	}
	if out := os.Getenv("PDF_OUT"); out != "" {
		_ = os.WriteFile(out, b, 0o644)
	}
}

func TestWrapAndWidth(t *testing.T) {
	if w := Width(Regular, 10, "Hello"); w < 20 || w > 30 {
		t.Fatalf("width = %v", w)
	}
	lines := Wrap(Regular, 10, "Supply and installation of four colour multifunction copiers at the head office", 120)
	if len(lines) < 3 {
		t.Fatalf("lines = %q", lines)
	}
	for _, l := range lines {
		if Width(Regular, 10, l) > 120 {
			t.Fatalf("line too wide: %q", l)
		}
	}
	if got := Wrap(Regular, 10, "a\n\nb", 100); len(got) != 3 {
		t.Fatalf("paragraphs = %q", got)
	}
}
