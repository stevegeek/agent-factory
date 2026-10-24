package hostrun

import (
	"encoding/json"
	"os"
	"testing"
)

// port: TF LookTest.test_screen_drops_scion_noise_and_keeps_the_end
func TestScreenDropsScionNoiseAndKeepsTheEnd(t *testing.T) {
	text := "2026/10/16 20:12:08 WARN settings file contains unrecognized keys\none\ntwo\nthree\n\n"
	if got := ScreenText(text, 2); got != "two\nthree" {
		t.Errorf("%q", got)
	}
	// \d is any decimal; the noise needs its final space
	if got := ScreenText("٢٠٢٦/١٠/١٧ ٠٨:٠٠:٠٠ WARN x\n2026/10/17 08:00:00 WARN\n 2026/10/17 08:00:00 WARN x\nkeep", 0); got != "2026/10/17 08:00:00 WARN\n 2026/10/17 08:00:00 WARN x\nkeep" {
		t.Errorf("%q", got)
	}
}

// screen_text over the golden corpus (the Python goldens tool).
func TestScreenGolden(t *testing.T) {
	b, err := os.ReadFile("testdata/py/screen.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Rows []struct {
			In    string
			Lines int
			Out   string
		}
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Rows) < 10 {
		t.Fatalf("%d rows", len(doc.Rows))
	}
	for _, r := range doc.Rows {
		if got := ScreenText(r.In, r.Lines); got != r.Out {
			t.Errorf("%q/%d: %q, want %q", r.In, r.Lines, got, r.Out)
		}
	}
}
