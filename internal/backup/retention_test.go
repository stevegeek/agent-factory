package backup

import (
	"slices"
	"sort"
	"testing"
	"time"
)

// §15.2: retention over a calendar of daily stamps: the last 7 days, the
// last 4 Sundays and the last 6 firsts of a month.
func TestRetentionOverACalendar(t *testing.T) {
	var names []string
	start := time.Date(2026, 1, 1, 4, 30, 0, 0, time.UTC) // a Thursday
	for d := 0; d < 300; d++ {
		names = append(names, archiveName("box", start.AddDate(0, 0, d)))
	}
	// a second archive on the newest day: only the newest of a day counts
	last := start.AddDate(0, 0, 299)
	names = append(names, archiveName("box", last.Add(-time.Hour)), "factory-box-x.tar.gz.fbk.sha256", "notes.txt")
	keep, drop := DefaultRetention().keep(names)

	var want []string
	for d := 0; d < 7; d++ {
		want = append(want, archiveName("box", last.AddDate(0, 0, -d)))
	}
	sundays := 0
	for d := 0; sundays < 4; d++ {
		if day := last.AddDate(0, 0, -d); day.Weekday() == time.Sunday {
			sundays++
			want = append(want, archiveName("box", day))
		}
	}
	firsts := 0
	for d := 0; firsts < 6; d++ {
		if day := last.AddDate(0, 0, -d); day.Day() == 1 {
			firsts++
			want = append(want, archiveName("box", day))
		}
	}
	slices.Sort(want)
	want = slices.Compact(want)
	got := slices.Clone(keep)
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("kept\n%v\nwant\n%v", got, want)
	}
	if len(keep)+len(drop) != 301 {
		t.Errorf("kept %d + dropped %d of 301 archives (the sidecar and notes are neither)", len(keep), len(drop))
	}
	for _, n := range drop {
		if slices.Contains(want, n) {
			t.Errorf("dropped a kept archive %s", n)
		}
	}
}

func TestRetentionKeepsEverythingWhileYoung(t *testing.T) {
	start := time.Date(2026, 10, 1, 4, 30, 0, 0, time.UTC)
	var names []string
	for d := 0; d < 5; d++ {
		names = append(names, archiveName("box", start.AddDate(0, 0, d)))
	}
	if _, drop := DefaultRetention().keep(names); len(drop) != 0 {
		t.Errorf("dropped %v", drop)
	}
}

func TestArchiveNames(t *testing.T) {
	at := time.Date(2026, 10, 18, 4, 30, 5, 0, time.FixedZone("x", 3600))
	n := archiveName("my-host", at)
	if n != "factory-my-host-20261018T033005Z.tar.gz.fbk" {
		t.Errorf("name %q", n)
	}
	h, got, ok := parseName(n)
	if !ok || h != "my-host" || !got.Equal(at) {
		t.Errorf("parse: %q %v %v", h, got, ok)
	}
	for _, bad := range []string{n + ".sha256", "factory-x-2026.tar.gz.fbk", "other"} {
		if _, _, ok := parseName(bad); ok {
			t.Errorf("parsed %q", bad)
		}
	}
}
