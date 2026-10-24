package pull

import (
	"context"
	"errors"
	"io/fs"
	"slices"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/vitals"
)

func (w *world) vitalsPosts() int {
	n := 0
	for _, a := range w.ssh {
		if slices.Contains(a, "vitals") {
			n++
		}
	}
	return n
}

// Every Cfg.Vitals the desktop posts its vitals, which the dispatcher
// stamps with its own clock; 0 posts none.
func TestVitalsArePostedOnTheirInterval(t *testing.T) {
	w := newWorld(t)
	w.step()
	if w.vitalsPosts() != 0 {
		t.Fatalf("vitals with Vitals 0: %q", w.ssh)
	}
	w.p.Cfg.Vitals = time.Minute
	taken := t0.Add(-time.Hour) // the desktop's clock is off
	w.p.Vitals = func(context.Context) vitals.Vitals {
		return vitals.Vitals{Taken: taken, Hostname: "desktop", Load: []float64{1, 2, 3}, CPUs: 16}
	}
	w.step()
	if w.vitalsPosts() != 1 {
		t.Fatalf("no vitals post: %q", w.ssh)
	}
	v, err := w.factory.Vitals("desktop")
	if err != nil || v.Hostname != "desktop" || v.CPUs != 16 || !v.Reported.Equal(taken) || v.Taken.Before(t0) {
		t.Errorf("on the dispatcher %+v %v", v, err)
	}
	w.clk.Advance(30 * time.Second)
	w.step()
	if w.vitalsPosts() != 1 {
		t.Errorf("posted again within the interval")
	}
	w.clk.Advance(31 * time.Second)
	w.step()
	if w.vitalsPosts() != 2 {
		t.Errorf("not posted again after the interval")
	}
}

// A post that fails or is refused is logged; the pass goes on.
func TestAFailedVitalsPostIsNotFatal(t *testing.T) {
	w := newWorld(t)
	w.p.Cfg.Vitals = time.Minute
	w.p.Vitals = func(context.Context) vitals.Vitals { return vitals.Vitals{Load: []float64{1, 2, 3, 4}} } // refused
	w.step()
	if _, err := w.factory.Vitals("desktop"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused post stored: %v", err)
	}
	inner := w.p.Exec
	w.p.Exec = execx.Func(func(ctx context.Context, c execx.Cmd) (execx.Result, error) {
		if slices.Contains(c.Argv, "vitals") {
			return execx.Result{}, errors.New("ssh: no route")
		}
		return inner.Run(ctx, c)
	})
	w.clk.Advance(time.Minute)
	if err := w.p.Step(context.Background()); err != nil {
		t.Errorf("step: %v", err)
	}
}

// With no Vitals func, the desktop's own are collected, its home's disk
// among them.
func TestVitalsCollectedByDefault(t *testing.T) {
	w := newWorld(t)
	w.p.Cfg.Vitals = time.Minute
	w.p.Home = t.TempDir()
	w.step()
	v, err := w.factory.Vitals("desktop")
	if err != nil || len(v.Disks) == 0 || v.Disks[0].Path != "/" || v.Containers != nil {
		t.Errorf("collected %+v %v", v, err)
	}
}
