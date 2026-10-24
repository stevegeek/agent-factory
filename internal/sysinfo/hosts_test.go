package sysinfo

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/vitals"
)

// Record asks every worker host for its vitals, all at once; a host that
// gives none keeps its last vitals, with the error beside them.
func TestRecordAsksTheHosts(t *testing.T) {
	d, p := collectDeps(t)
	cfg, err := config.Parse([]byte("max_agents = 3\n[hosts.helpera]\nssh = \"factory@helpera\"\nslots = 2\n[hosts.desk]\ntransport = \"pull\"\nslots = 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	d.Config = cfg
	var mu sync.Mutex
	asked := map[string]int{}
	fail := map[string]error{}
	d.HostVitals = func(ctx context.Context, h config.HostCfg) (*vitals.Vitals, error) {
		mu.Lock()
		defer mu.Unlock()
		asked[h.Name]++
		if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > VitalsTimeout {
			t.Errorf("%s: deadline %v %v", h.Name, dl, ok)
		}
		if err := fail[h.Name]; err != nil {
			return nil, err
		}
		return &vitals.Vitals{Taken: now, Hostname: h.Name + "-box", Load: []float64{1, 1, 1}}, nil
	}
	if err := Record(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	s, _ := Read(File(p))
	byName := map[string]Host{}
	for _, h := range s.Hosts {
		byName[h.Name] = h
	}
	if asked["helpera"] != 1 || asked["desk"] != 1 || len(asked) != 2 {
		t.Errorf("asked %v", asked)
	}
	if v := byName["helpera"].Vitals; v == nil || v.Hostname != "helpera-box" || byName["helpera"].VitalsError != "" || byName["helpera"].Pull {
		t.Errorf("helpera %+v", byName["helpera"])
	}
	if h := byName["desk"]; h.Vitals == nil || !h.Pull {
		t.Errorf("desk %+v", h)
	}
	if byName["local"].Vitals != nil {
		t.Error("the dispatcher asked itself")
	}

	// helpera does not answer this pass: its last vitals stay, desk has none to keep
	fail["helpera"] = errors.New("factory@helpera vitals timed out")
	fail["desk"] = errors.New("no report yet")
	d.Previous = &Snapshot{Hosts: []Host{{Name: "helpera", Vitals: byName["helpera"].Vitals}}}
	if err := Record(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	s, _ = Read(File(p))
	for _, h := range s.Hosts {
		byName[h.Name] = h
	}
	if h := byName["helpera"]; h.Vitals == nil || h.Vitals.Hostname != "helpera-box" || h.VitalsError != "factory@helpera vitals timed out" {
		t.Errorf("helpera carried over %+v", h)
	}
	if h := byName["desk"]; h.Vitals != nil || h.VitalsError != "no report yet" {
		t.Errorf("desk %+v", h)
	}
}

// A host that hangs costs VitalsTimeout, no more, and is an error; a
// panic is one too.
func TestAHungHostIsCutOff(t *testing.T) {
	d, _ := collectDeps(t)
	d.HostVitals = func(ctx context.Context, h config.HostCfg) (*vitals.Vitals, error) { select {} }
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	got := askHosts(ctx, d)
	if time.Since(start) > time.Second || got["helpera"].err == nil || !strings.Contains(got["helpera"].err.Error(), "no answer") {
		t.Errorf("hung host: %v after %v", got, time.Since(start))
	}
	d.HostVitals = func(context.Context, config.HostCfg) (*vitals.Vitals, error) { panic("boom") }
	if got := askHosts(t.Context(), d); got["helpera"].err == nil || !strings.Contains(got["helpera"].err.Error(), "boom") {
		t.Errorf("panic: %v", got)
	}
	d.HostVitals = nil
	if got := askHosts(t.Context(), d); got != nil {
		t.Errorf("asked with no HostVitals: %v", got)
	}
}
