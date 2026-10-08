package numbering

import "testing"

func TestRender(t *testing.T) {
	cases := []struct {
		f      Format
		period int
		n      int64
		want   string
	}{
		{formats[Quotation], 2026, 1, "DOS-Q-2026-0001"},
		{formats[Invoice], 2026, 12345, "DOS-INV-2026-12345"},
		{formats[Client], 0, 42, "DOS-CL-00042"},
	}
	for _, c := range cases {
		if got := Render(c.f, c.period, c.n); got != c.want {
			t.Errorf("Render = %s, want %s", got, c.want)
		}
	}
	if got := Revision("DOS-Q-2026-0001", 1); got != "DOS-Q-2026-0001-R1" {
		t.Errorf("Revision = %s", got)
	}
}
