package scion

import "testing"

// A pinned file mounts writable from its source, a directory read-only, at
// the same place in the agent's workspace; a path outside the project, a
// relative source, or an odd agent name, is refused.
func TestPinnedVolumesMountIntoTheWorkspace(t *testing.T) {
	got, err := PinnedVolumes("t0003-w1", []Pinned{
		{Rel: "Gemfile.lock", Source: "/f/run/pinned/t0003-w1/Gemfile.lock"},
		{Rel: "lib/", Source: "/f/projects/p/lib", ReadOnly: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `  - type: local
    source: "/f/run/pinned/t0003-w1/Gemfile.lock"
    target: "/repo-root/.scion/agents/t0003-w1/workspace/Gemfile.lock"
  - type: local
    source: "/f/projects/p/lib"
    target: "/repo-root/.scion/agents/t0003-w1/workspace/lib"
    read_only: true
`
	if got != want {
		t.Errorf("volumes\n%s\nwant\n%s", got, want)
	}
	for _, rel := range []string{"../x", "/etc/passwd", ""} {
		if _, err := PinnedVolumes("t0003-w1", []Pinned{{Rel: rel, Source: "/f/x"}}); err == nil {
			t.Errorf("%q accepted", rel)
		}
	}
	if _, err := PinnedVolumes("t0003-w1", []Pinned{{Rel: "Gemfile.lock", Source: "run/x"}}); err == nil {
		t.Error("a relative source accepted")
	}
	if _, err := PinnedVolumes("../x", []Pinned{{Rel: "Gemfile.lock", Source: "/f/x"}}); err == nil {
		t.Error("an odd agent name accepted")
	}
}
