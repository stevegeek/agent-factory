package deploy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestHealthyWaitsForTheNewVersion: the old Foreman answers ok until its
// restart; only the new version passes, and a version that never comes
// times out.
func TestHealthyWaitsForTheNewVersion(t *testing.T) {
	t.Parallel() // it waits out a check
	f := newFixture(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) <= 3 {
			fmt.Fprintf(w, "ok\nversion %s\n", f.a)
			return
		}
		fmt.Fprintf(w, "ok\nversion %s\nexposed: nothing\n", f.b)
	}))
	defer srv.Close()
	f.write("config.toml", "[foreman]\nlisten = \""+strings.TrimPrefix(srv.URL, "http://")+"\"\n")
	f.install(f.b)
	if err := f.s.Healthy(context.Background(), f.b, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if hits.Load() < 4 {
		t.Errorf("passed after %d answers, before the new version", hits.Load())
	}
	// the old Foreman never restarts
	stuck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "ok\nversion %s\n", f.a)
	}))
	defer stuck.Close()
	f.write("config.toml", "[foreman]\nlisten = \""+strings.TrimPrefix(stuck.URL, "http://")+"\"\n")
	err := f.s.Healthy(context.Background(), f.b, time.Time{})
	if err == nil || !strings.Contains(err.Error(), "did not name version "+f.b) {
		t.Errorf("Healthy = %v", err)
	}
}

func TestHealthyChecksTheFactoryBinary(t *testing.T) {
	f := newFixture(t)
	f.install(f.a) // Foreman says b below, bin/factory says a
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "ok\nversion %s\n", f.b)
	}))
	defer srv.Close()
	f.write("config.toml", "[foreman]\nlisten = \"0.0.0.0:"+srv.URL[strings.LastIndex(srv.URL, ":")+1:]+"\"\n")
	err := f.s.Healthy(context.Background(), f.b, time.Time{})
	if err == nil || !strings.Contains(err.Error(), "bin/factory version is") {
		t.Errorf("Healthy = %v", err)
	}
}

func TestHealthyAfterRestoreReadsTheRestoredListen(t *testing.T) {
	t.Parallel() // it waits out a check
	f := newFixture(t)
	// the snapshot's config points at the fixture's Foreman, which names a
	if err := Snapshot(f.p, f.b, f.a); err != nil {
		t.Fatal(err)
	}
	wrong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok\nversion 999999999999")
	}))
	defer wrong.Close()
	f.write("config.toml", "[foreman]\nlisten = \""+strings.TrimPrefix(wrong.URL, "http://")+"\"\n")
	if err := f.s.Healthy(context.Background(), f.a, time.Time{}); err == nil {
		t.Fatal("the new listen answered for a")
	}
	if _, err := f.s.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Healthy(context.Background(), f.a, time.Time{}); err != nil {
		t.Errorf("after the restore: %v", err)
	}
}

// TestHealthyVersionCheckHasItsOwnTime: a Foreman that names the version
// at the end of the wait still leaves bin/factory version its own time.
func TestHealthyVersionCheckHasItsOwnTime(t *testing.T) {
	t.Parallel() // it waits out most of a check
	f := newFixture(t)
	f.install(f.b)
	f.write("bin/factory", fmt.Sprintf("#!/bin/sh\nsleep 0.6\necho \"%s go1.test\"\n", f.b))
	f.s.HealthWait = 3 * time.Second
	start := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if time.Since(start) < f.s.HealthWait-300*time.Millisecond {
			fmt.Fprintf(w, "ok\nversion %s\n", f.a)
			return
		}
		fmt.Fprintf(w, "ok\nversion %s\n", f.b)
	}))
	defer srv.Close()
	f.write("config.toml", "[foreman]\nlisten = \""+strings.TrimPrefix(srv.URL, "http://")+"\"\n")
	if err := f.s.Healthy(context.Background(), f.b, time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func TestLoopback(t *testing.T) {
	for in, want := range map[string]string{"0.0.0.0:8790": "127.0.0.1:8790", "[::]:8790": "[::1]:8790", "127.0.0.1:1": "127.0.0.1:1", "box:2": "box:2"} {
		if got := loopback(in); got != want {
			t.Errorf("loopback(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHealthOK(t *testing.T) {
	since := time.Date(2026, 1, 2, 3, 4, 5, 500_000_000, time.UTC)
	for _, c := range []struct {
		name, body string
		since      time.Time
		ok         bool
	}{
		{"the version", "ok\nversion abc\n", time.Time{}, true},
		{"another version", "ok\nversion def\n", time.Time{}, false},
		{"not ok", "broken\nversion abc\n", time.Time{}, false},
		{"started after", "ok\nversion abc\nstarted 2026-01-02T03:04:06Z\n", since, true},
		{"started in the same second", "ok\nversion abc\nstarted 2026-01-02T03:04:05Z\nexposed: x\n", since, true},
		{"started before", "ok\nversion abc\nstarted 2026-01-02T03:04:04Z\n", since, false},
		{"no start", "ok\nversion abc\n", since, false},
		{"a start that does not read", "ok\nversion abc\nstarted soon\n", since, false},
	} {
		if got := healthOK(c.body, "abc", c.since); got != c.ok {
			t.Errorf("%s: healthOK = %v, want %v", c.name, got, c.ok)
		}
	}
}

// TestHealthyWaitsForAForemanStartedSince: with since set, a Foreman of
// the right version started before it does not pass.
func TestHealthyWaitsForAForemanStartedSince(t *testing.T) {
	t.Parallel() // it waits out a check
	f := newFixture(t)
	since := time.Now()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started := since.Add(-time.Hour)
		if hits.Add(1) > 3 {
			started = since.Add(time.Second)
		}
		fmt.Fprintf(w, "ok\nversion %s\nstarted %s\n", f.a, started.UTC().Format(time.RFC3339))
	}))
	defer srv.Close()
	f.write("config.toml", "[foreman]\nlisten = \""+strings.TrimPrefix(srv.URL, "http://")+"\"\n")
	if err := f.s.Healthy(context.Background(), f.a, since); err != nil {
		t.Fatal(err)
	}
	if hits.Load() < 4 {
		t.Errorf("passed after %d answers, before the restart", hits.Load())
	}
	f.s.HealthWait = 500 * time.Millisecond
	err := f.s.Healthy(context.Background(), f.a, since.Add(time.Hour))
	if err == nil || !strings.Contains(err.Error(), "started after") {
		t.Errorf("Healthy = %v", err)
	}
}
