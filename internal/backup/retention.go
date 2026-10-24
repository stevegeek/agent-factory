package backup

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

// Retention is how many archives are kept: the newest Daily days,
// the newest Weekly Sundays and the newest Monthly 1sts of a month, each
// day counted once (its newest archive).
type Retention struct {
	Daily, Weekly, Monthly int
}

// DefaultRetention is 7 daily, 4 weekly, 6 monthly.
func DefaultRetention() Retention { return Retention{Daily: 7, Weekly: 4, Monthly: 6} }

// stampLayout is the archive name's stamp: UTC, no separators that an
// object key would need to escape.
const stampLayout = "20060102T150405Z"

// archiveSuffix ends every archive's name; the sidecar adds sidecarSuffix.
const (
	archiveSuffix = ".tar.gz.fbk"
	sidecarSuffix = ".sha256"
)

// archiveName is factory-<host>-<stamp>.tar.gz.fbk.
func archiveName(host string, t time.Time) string {
	return "factory-" + host + "-" + t.UTC().Format(stampLayout) + archiveSuffix
}

var nameRE = regexp.MustCompile(`^factory-(.+)-(\d{8}T\d{6}Z)\.tar\.gz\.fbk$`) // re: go

// parseName is an archive name's host and time (ok false for anything
// else, a sidecar included).
func parseName(name string) (host string, t time.Time, ok bool) {
	m := nameRE.FindStringSubmatch(name)
	if m == nil {
		return "", time.Time{}, false
	}
	t, err := time.Parse(stampLayout, m[2])
	if err != nil {
		return "", time.Time{}, false
	}
	return m[1], t, true
}

// keep returns the archive names to keep, of names (archives of one host).
// Names that are not archives are never in either list.
func (r Retention) keep(names []string) (keep, drop []string) {
	type arch struct {
		name string
		t    time.Time
	}
	var all []arch
	for _, n := range names {
		if _, t, ok := parseName(n); ok {
			all = append(all, arch{n, t})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].t.After(all[j].t) })
	kept := map[string]bool{}
	pick := func(limit int, match func(time.Time) bool) {
		days := map[string]bool{}
		for _, a := range all {
			if len(days) >= limit {
				return
			}
			day := a.t.Format("2006-01-02")
			if !match(a.t) || days[day] {
				continue
			}
			days[day] = true
			kept[a.name] = true
		}
	}
	pick(r.Daily, func(time.Time) bool { return true })
	pick(r.Weekly, func(t time.Time) bool { return t.Weekday() == time.Sunday })
	pick(r.Monthly, func(t time.Time) bool { return t.Day() == 1 })
	for _, a := range all {
		if kept[a.name] {
			keep = append(keep, a.name)
		} else {
			drop = append(drop, a.name)
		}
	}
	return keep, drop
}

// baseName is the part of an object key after the prefix.
func baseName(key string) string {
	if i := strings.LastIndex(key, "/"); i >= 0 {
		return key[i+1:]
	}
	return key
}
