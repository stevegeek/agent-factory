package jobproto

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/jsonx"
)

// A helper that predates project, toolchain and base reads what today's
// dispatcher sends as it read meta.json before: the same job, run cold.
// checkMetaWithoutProject below is CheckMeta as it was then, word for word,
// so a later change to its lookup rules (a strict decoder, say) shows here.
func TestAnOlderHelperReadsTodaysMeta(t *testing.T) {
	base := strings.Repeat("ab", 20)
	for _, m := range []Meta{
		{Branch: "bot/x", Start: "main", Model: "sonnet", Bundle: true, TimeoutMinutes: 30, Project: "p", Toolchain: "go", Base: base},
		{Branch: "gate", Start: base, Model: "sonnet", TimeoutMinutes: 30, Kind: KindGate, Project: "p", Toolchain: "go", Base: base},
		{Branch: "bot/x", Start: "main", Model: "sonnet", TimeoutMinutes: 30, Harness: "pi", Project: "p"},
	} {
		raw := m.Encode()
		v, err := jsonx.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		old, err := checkMetaWithoutProject(v)
		if err != nil {
			t.Errorf("%s: an older helper refuses it: %v", raw, err)
			continue
		}
		now, err := CheckMeta(v)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		now.Project, now.Toolchain, now.Base = "", "", ""
		if old != now {
			t.Errorf("%s: an older helper reads %+v, want %+v", raw, old, now)
		}
	}
}

func checkMetaWithoutProject(v jsonx.Value) (Meta, error) {
	o, ok := v.Object()
	if !ok {
		return Meta{}, ErrNotObject
	}
	var m Meta
	for _, k := range []string{"branch", "start"} {
		s := ""
		if val, ok := o.Lookup(k); ok {
			s = jsonx.PyStr(val)
		}
		if !branchName.MatchString(s) || strings.Contains(s, "..") || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "/") {
			return Meta{}, fmt.Errorf("bad %s in meta.json", k)
		}
		if k == "branch" {
			m.Branch = s
		} else {
			m.Start = s
		}
	}
	m.Model = DefaultModel
	if val := o.Get("model"); jsonx.Truthy(val) {
		m.Model = jsonx.PyStr(val)
	}
	if !modelName.MatchString(m.Model) || strings.HasPrefix(m.Model, "-") {
		return Meta{}, errors.New("bad model in meta.json")
	}
	m.TimeoutMinutes = DefaultTimeoutMinutes
	if val, ok := o.Lookup("timeout_minutes"); ok {
		n, err := pyInt(val)
		switch {
		case errors.Is(err, errOverflow):
			return Meta{}, errors.New("bad timeout_minutes in meta.json")
		case err == nil:
			m.TimeoutMinutes = clampMinutes(n)
		}
	}
	m.Bundle = jsonx.Truthy(o.Get("bundle"))
	m.Harness = DefaultHarness
	if val := o.Get("harness"); jsonx.Truthy(val) {
		h := jsonx.PyStr(val)
		if !harnessOK(h) {
			return Meta{}, errors.New("bad harness in meta.json")
		}
		m.Harness = h
	}
	if val, ok := o.Lookup("kind"); ok {
		switch k := jsonx.PyStr(val); k {
		case KindAgent:
		case KindGate:
			m.Kind = k
		default:
			return Meta{}, errors.New("bad kind in meta.json")
		}
	}
	if m.Kind == KindGate && !commitID.MatchString(m.Start) {
		return Meta{}, errors.New("bad start in meta.json: a gate job starts from a commit id")
	}
	return m, nil
}
