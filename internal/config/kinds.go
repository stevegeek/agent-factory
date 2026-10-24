package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/stevegeek/agent-factory/internal/textx"
)

// ProjectKind is a project kind: the toolchain a project builds with and
// the defaults a project of the kind takes for the keys its own table
// leaves out.
type ProjectKind struct {
	Name, Toolchain, Language string
	Install, Test, Lint       string
	GateMemory                string
	PinnedUntracked           []string
	PinnedSet                 bool   // the kind sets pinned_untracked (an empty list counts)
	From                      string // the built-in a config kind starts from; "" for none
	Source                    string // KindCore or KindConfig
}

// Where a kind comes from: built into core, or [kinds.<name>] in config
// (which may override a built-in of the same name).
const (
	KindCore   = "core"
	KindConfig = "config"
)

// BuiltinKinds are the kinds core ships, as a new slice.
func BuiltinKinds() []ProjectKind {
	gem := func(name, language string) ProjectKind {
		return ProjectKind{Name: name, Toolchain: "ruby", Language: language,
			Install: "bundle check", Test: "bundle exec rake test", Lint: "bundle exec standardrb",
			PinnedUntracked: []string{"Gemfile.lock"}, PinnedSet: true, Source: KindCore}
	}
	return []ProjectKind{
		gem("ruby-gem", "Ruby gem"),
		gem("rails-gem", "Ruby gem for Rails"),
		{Name: "crystal-shard", Toolchain: "crystal", Language: "Crystal shard",
			Install: "shards check || shards install", Test: "crystal spec",
			Lint:            "crystal tool format --check src spec && ameba src spec",
			PinnedUntracked: []string{"lib", "shard.lock"}, PinnedSet: true, Source: KindCore},
	}
}

// ToolchainNames are the toolchains a kind may name.
func ToolchainNames() []string { return []string{"ruby", "crystal"} }

func builtinKind(name string) (ProjectKind, bool) {
	for _, k := range BuiltinKinds() {
		if k.Name == name {
			return k, true
		}
	}
	return ProjectKind{}, false
}

// Kinds are the built-in kinds (each one [kinds.<name>] overrides replaced
// in place), then the kinds only config has, in file order. The slice is
// new.
func (c *Config) Kinds() []ProjectKind {
	out := BuiltinKinds()
	for _, k := range c.kinds {
		k.PinnedUntracked = slices.Clone(k.PinnedUntracked)
		if i := slices.IndexFunc(out, func(b ProjectKind) bool { return b.Name == k.Name }); i >= 0 {
			out[i] = k
		} else {
			out = append(out, k)
		}
	}
	return out
}

// KindNamed is the kind called name, from Kinds.
func (c *Config) KindNamed(name string) (ProjectKind, bool) {
	for _, k := range c.Kinds() {
		if k.Name == name {
			return k, true
		}
	}
	return ProjectKind{}, false
}

// KindOf is the project's kind: its `kind`, or with none the kind its
// language suggests (crystal-shard when it says "crystal", else ruby-gem),
// with inferred true. An unknown kind is the zero ProjectKind (Problems
// reports it). An inferred kind fills no keys; it only answers Toolchain,
// and it is always the built-in, so a [kinds] override cannot change a
// project that has no `kind`.
func (c *Config) KindOf(p Project) (k ProjectKind, inferred bool) {
	if p.Kind != "" {
		k, _ = c.KindNamed(p.Kind)
		return k, false
	}
	name := "ruby-gem"
	if strings.Contains(textx.Lower(p.Language), "crystal") {
		name = "crystal-shard"
	}
	k, _ = builtinKind(name)
	return k, true
}

// Toolchain is the toolchain of the project's kind (KindOf); "" for an
// unknown kind.
func (c *Config) Toolchain(p Project) string {
	k, _ := c.KindOf(p)
	return k.Toolchain
}

// decodeKind is one [kinds.<name>]: the built-in it names in `from`, or
// else the built-in of its own name, or else nothing, with the keys the
// table sets laid over it.
func decodeKind(name string, s *section) ProjectKind {
	from := s.str("from", "")
	base, _ := builtinKind(name)
	if from != "" {
		base, _ = builtinKind(from)
	}
	k := base
	k.Name, k.From, k.Source = name, from, KindConfig
	k.PinnedUntracked = slices.Clone(base.PinnedUntracked)
	for _, f := range []struct {
		key string
		to  *string
	}{{"toolchain", &k.Toolchain}, {"language", &k.Language}, {"install", &k.Install},
		{"test", &k.Test}, {"lint", &k.Lint}, {"gate_memory", &k.GateMemory}} {
		if v, ok := s.optStr(f.key); ok {
			*f.to = v
		}
	}
	if l, ok := s.strListSet("pinned_untracked"); ok {
		k.PinnedUntracked, k.PinnedSet = l, true
	}
	return k
}

// optStr is a string key the table may leave out: false when absent or
// not a string (which is reported as mistyped).
func (s *section) optStr(key string) (string, bool) {
	if _, ok := s.t.Get(key); !ok {
		s.used[key] = true
		return "", false
	}
	before := len(s.c.Mistyped)
	x := s.str(key, "")
	return x, len(s.c.Mistyped) == before
}

// resolveKinds fills, for each factory project with a known `kind`, the
// keys its table leaves out and its kind gives, and records them so Has
// counts them. A project with no kind is left as it is.
func (c *Config) resolveKinds() {
	for i := range c.Projects {
		p := &c.Projects[i]
		if p.Mode != ModeFactory || p.Kind == "" {
			continue
		}
		k, ok := c.KindNamed(p.Kind)
		if !ok {
			continue
		}
		for _, f := range []struct {
			key      string
			from, to *string
		}{{"language", &k.Language, &p.Language}, {"install", &k.Install, &p.Install}, {"test", &k.Test, &p.Test},
			{"lint", &k.Lint, &p.Lint}, {"gate_memory", &k.GateMemory, &p.GateMemory}} {
			if !p.Has(f.key) && *f.from != "" {
				*f.to = *f.from
				p.fromKind = append(p.fromKind, f.key)
			}
		}
		if !p.Has("pinned_untracked") && k.PinnedSet {
			p.PinnedUntracked = slices.Clone(k.PinnedUntracked)
			if p.PinnedUntracked == nil {
				p.PinnedUntracked = []string{}
			}
			p.fromKind = append(p.fromKind, "pinned_untracked")
		}
	}
}

// kindProblems are the problems of [kinds.*] and of the factory projects'
// `kind`.
func (c *Config) kindProblems() []string {
	var out []string
	for _, k := range c.kinds {
		label := "[kinds." + k.Name + "]"
		_, builtin := builtinKind(k.From)
		switch {
		case k.From != "" && !builtin:
			out = append(out, fmt.Sprintf("%s from = %q: not a built-in kind", label, k.From))
		case k.Toolchain == "":
			out = append(out, label+": no toolchain (set toolchain or from)")
		case !slices.Contains(ToolchainNames(), k.Toolchain):
			out = append(out, fmt.Sprintf("%s toolchain = %q: one of %s", label, k.Toolchain, strings.Join(ToolchainNames(), ", ")))
		}
	}
	for _, p := range c.FactoryProjects() {
		if _, ok := c.KindNamed(p.Kind); p.Kind != "" && !ok {
			out = append(out, fmt.Sprintf("[projects.%s] kind = %q: no such kind (factory kinds lists them)", p.Key, p.Kind))
		}
	}
	return out
}
