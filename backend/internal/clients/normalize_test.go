package clients

import "testing"

func TestNameKey(t *testing.T) {
	cases := map[string]string{
		"ABC Mining Ltd.":                  "abc mining",
		"abc  mining limited":              "abc mining",
		"The ABC Mining Company Limited":   "abc mining",
		"Smith & Sons (Zambia) Ltd":        "smith and sons zambia",
		"Smith and Sons Zambia":            "smith and sons zambia",
		"Ltd":                              "ltd",
		"Kansanshi Mining PLC":             "kansanshi mining",
		"Digital Office Solutions Limited": "digital office solutions",
	}
	for in, want := range cases {
		if got := NameKey(in); got != want {
			t.Errorf("NameKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormID(t *testing.T) {
	s := " 120-2019/001 "
	if got := *normID(&s); got != "1202019001" {
		t.Errorf("normID = %q", got)
	}
	empty := " - "
	if normID(&empty) != nil {
		t.Error("expected nil for empty id")
	}
}
