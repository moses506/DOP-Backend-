package quotes

import "testing"

func TestParseAndFormat(t *testing.T) {
	for in, want := range map[string]hundredths{"1250": 125000, "1,250.5": 125050, "0.05": 5, "12 500.00": 1250000, ".5": 50} {
		got, err := parseHundredths(in)
		if err != nil || got != want {
			t.Errorf("parse %q = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "-1", "1.234", "abc", "1e5"} {
		if _, err := parseHundredths(bad); err == nil {
			t.Errorf("parse %q should fail", bad)
		}
	}
	if s := hundredths(123456789).Grouped(); s != "1,234,567.89" {
		t.Errorf("grouped = %s", s)
	}
	if s := hundredths(1250).Trimmed(); s != "12.5" {
		t.Errorf("trimmed = %s", s)
	}
	if s := hundredths(200).Trimmed(); s != "2" {
		t.Errorf("trimmed = %s", s)
	}
}

func TestLineAmounts(t *testing.T) {
	// 3 × 12,500.00 less 5%, VAT 16%
	g, n, v, tot := lineAmounts(300, 1250000, 500, 1600)
	if g != 3750000 || n != 3562500 || v != 570000 || tot != 4132500 {
		t.Fatalf("got %v %v %v %v", g, n, v, tot)
	}
	// Rounding half up: 1 × 0.05 less 50% = 0.025 → 0.03
	if _, n, _, _ := lineAmounts(100, 5, 5000, 0); n != 3 {
		t.Fatalf("rounding = %v", n)
	}
	// 2.5 hours at 333.33, VAT 16%: net 833.33 (833.325 rounds up), VAT 133.33
	if _, n, v, _ := lineAmounts(250, 33333, 0, 1600); n != 83333 || v != 13333 {
		t.Fatalf("hours = %v %v", n, v)
	}
	if p := percentOf(187500, 3900000); p != 481 {
		t.Fatalf("percent = %v", p)
	}
}
