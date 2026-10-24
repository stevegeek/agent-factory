// Package tiers is the rights checker. It judges a manager's config draft from the files it
// really changes, old against new as applied to config main, and gives the
// tier the tick acts on: auto (the tick applies it by itself), tap (it
// waits for the maintainer's "Apply it") or never (the tick refuses it).
// The manager's own claim about its draft plays no part.
//
// Auto is a narrow allowlist: a place the rules do not know, or a change
// with no place at all (comments or layout only), needs a tap. A draft
// takes the worst tier of its places. The checker is pure: it reads no
// filesystem and runs nothing.
package tiers

import (
	"path"
	"slices"
	"strings"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/toml"
)

// File is one file a draft touches, old and new as they are in the temp
// clone. Mode is the new mode `git apply --summary` reports ("" when the
// mode is unchanged or the file is deleted; a created file's ordinary
// "100644" is fine); OldMode is
// the file's mode on config main (`git ls-tree`, "" for a new file), since
// --summary prints nothing for an existing symlink whose target is edited;
// Binary is --numstat's "-". From is the source of a rename or copy
// (--summary's "rename a => b" / "copy a => b"); any rename or copy is
// refused, so the caller need not list the source separately.
type File struct {
	Path                     string // repo-relative, as the patch names it
	Old, New                 []byte
	Deleted, Created, Binary bool
	Mode, OldMode            string
	From                     string
}

// In is what a draft is judged on: the config repo's directory that holds
// config.toml (cfg.Deploy.ConfigDir), the factory's home (a new project's
// path must be <home>/projects/<key>), and the files.
type In struct {
	ConfigDir, FactoryHome string
	Files                  []File
}

// Verdict is the checker's answer. Places are every place the draft
// changes, Walls those that touch the factory's own safety (the desk flags
// them), Never those only the maintainer may change, and Reasons say why
// for each place and for the draft.
type Verdict struct {
	Tier                          model.Tier
	Places, Walls, Never, Reasons []string
}

// rank orders the tiers, worst last. An unknown tier ranks as never.
func rank(t model.Tier) int {
	switch t {
	case model.TierAuto:
		return 0
	case model.TierTap:
		return 1
	}
	return 2
}

// judge collects a verdict place by place.
type judge struct {
	v      Verdict
	places int
}

func (j *judge) add(place string, t model.Tier, wall bool, reason string) {
	j.places++
	if j.v.Tier == "" || rank(t) > rank(j.v.Tier) {
		j.v.Tier = t
	}
	if !slices.Contains(j.v.Places, place) {
		j.v.Places = append(j.v.Places, place)
	}
	if wall && !slices.Contains(j.v.Walls, place) {
		j.v.Walls = append(j.v.Walls, place)
	}
	if t == model.TierNever && !slices.Contains(j.v.Never, place) {
		j.v.Never = append(j.v.Never, place)
	}
	if reason != "" {
		j.v.Reasons = append(j.v.Reasons, reason)
	}
}

func (j *judge) note(reason string) { j.v.Reasons = append(j.v.Reasons, reason) }

// Draft judges a draft: the worst tier over all its files and places. No
// file, or no place, gives tap.
func Draft(in In) Verdict {
	j := &judge{}
	dir := strings.Trim(in.ConfigDir, "/")
	if dir == "." {
		dir = ""
	}
	for _, f := range in.Files {
		j.file(in, dir, f)
	}
	if j.places == 0 || j.v.Tier == "" {
		j.note("the draft changes no setting; a tap")
		if j.v.Tier == "" || rank(j.v.Tier) < rank(model.TierTap) {
			j.v.Tier = model.TierTap
		}
	}
	return j.v
}

// file judges one file by where it is, then config.toml place by place.
func (j *judge) file(in In, dir string, f File) {
	p := f.Path
	switch {
	case f.Binary:
		j.add(p, model.TierNever, false, p+": a binary file; only the maintainer adds one")
		return
	case f.From != "":
		j.add(p, model.TierNever, false, p+": renamed or copied from "+f.From+"; only the maintainer moves files")
		return
	case f.OldMode == "120000" || f.OldMode == "160000":
		j.add(p, model.TierNever, false, p+": already a symlink or a gitlink (mode "+f.OldMode+"); only the maintainer changes one")
		return
	case f.Mode != "" && !(f.Created && f.Mode == "100644"):
		j.add(p, model.TierNever, false, p+": mode "+f.Mode+" (a symlink or a mode change); only the maintainer makes one")
		return
	}
	rel, ok := under(p, dir)
	if !ok {
		j.add(p, model.TierNever, false, p+": outside "+configNames(dir)+"; only the maintainer changes it")
		return
	}
	switch {
	case rel == "config.toml" && f.Deleted:
		j.add(p, model.TierNever, false, p+": deleting the config; only the maintainer does that")
	case rel == "config.toml":
		j.config(in, p, f)
	case rel == "rules.md" && f.Deleted:
		j.add(p, model.TierNever, false, p+": deleting the rules; only the maintainer does that")
	case rel == "rules.md":
		j.add(p, model.TierTap, true, p+": the rules every agent works under; a tap")
	case strings.HasPrefix(rel, "context/") && len(rel) > len("context/"):
		j.add(p, model.TierTap, false, p+": a context note agents read as the maintainer's knowledge; a tap")
	default:
		j.add(p, model.TierNever, false, p+": outside "+configNames(dir)+"; only the maintainer changes it")
	}
}

// under is p relative to dir, when p is a clean relative path inside it.
func under(p, dir string) (string, bool) {
	if p == "" || strings.HasPrefix(p, "/") || path.Clean(p) != p || slices.Contains(strings.Split(p, "/"), "..") {
		return "", false
	}
	if dir == "" {
		return p, true
	}
	rel, ok := strings.CutPrefix(p, dir+"/")
	return rel, ok && rel != ""
}

func configNames(dir string) string {
	if dir != "" {
		dir += "/"
	}
	return dir + "config.toml, " + dir + "rules.md and " + dir + "context/"
}

// config judges a change of config.toml: the new config must parse and add
// no problem, then each place goes through Rule.
func (j *judge) config(in In, p string, f File) {
	newTab, err := toml.Parse(f.New)
	if err != nil {
		j.add(p, model.TierNever, false, p+": the new config does not parse: "+err.Error())
		return
	}
	oldTab := &toml.Table{}
	var oldProblems []string
	if !f.Created {
		if t, err := toml.Parse(f.Old); err == nil {
			oldTab = t
			oldCfg, _ := config.Parse(f.Old)
			oldProblems, _ = oldCfg.Problems(nil)
		} else {
			// every setting then counts as added, so each rule still runs
			j.note(p + ": the old config does not parse; every setting counts as new")
		}
	}
	newCfg, _ := config.Parse(f.New)
	problems, _ := newCfg.Problems(nil)
	for _, pr := range problems {
		if !slices.Contains(oldProblems, pr) {
			j.add("factory check: "+pr, model.TierNever, false, "the new config adds a problem: "+pr)
		}
	}
	changes, why := ConfigPlaces(oldTab, newTab)
	if why != "" {
		j.note(why)
	}
	if len(changes) == 0 {
		j.note(p + ": no setting changes (comments or layout only); a tap")
		j.add(p, model.TierTap, false, "")
		return
	}
	for _, c := range changes {
		t, wall, reason := Rule(c, in.FactoryHome)
		j.add(c.Place, t, wall, reason)
	}
	if lines := commentLines(f.Old, f.New); len(lines) > 0 {
		j.add(p+": comments or layout", model.TierTap, false, p+": comments or table headers added, removed or moved beside the settings ("+strings.Join(lines, " | ")+"); a tap, since every later manager run reads them")
	}
}

// commentLines are the lines a line-by-line diff of old and new adds or
// removes that carry text the settings do not: any line with a "#" (a
// comment, or a string holding one; the scan is deliberately not TOML-aware,
// which only widens what needs a tap) and any table header. A comment moved
// shows as removed and added, so it counts too. Blank lines and plain
// key = value lines are judged by their places.
func commentLines(old, new []byte) []string {
	a, b := strings.Split(string(old), "\n"), strings.Split(string(new), "\n")
	// lcs[i][j] is the longest common subsequence of a[i:] and b[j:]
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []string
	keep := func(l string) {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "#") || strings.HasPrefix(t, "[") {
			out = append(out, t)
		}
	}
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			i, j = i+1, j+1
		case j < len(b) && (i == len(a) || lcs[i][j+1] >= lcs[i+1][j]):
			keep(b[j])
			j++
		default:
			keep(a[i])
			i++
		}
	}
	return out
}
