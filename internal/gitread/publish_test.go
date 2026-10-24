package gitread

import (
	"slices"
	"testing"
)

// The commit messages a branch publishes are those from the base to the
// branch, newest first, each whole: subject and body.
func TestMessagesBaseToBranch(t *testing.T) {
	f := newFixture(t)
	f.repo.Write("CHANGELOG.md", "on main\n")
	f.repo.Commit("on main only")
	f.repo.Git("checkout", "-q", "-b", "bot/b")
	f.repo.Write("a.txt", "a\n")
	f.repo.Commit("first\n\nwith a body\nof two lines")
	f.repo.Write("b.txt", "b\n")
	f.repo.Commit("second")
	f.repo.Git("checkout", "-q", "main")
	got, err := f.r.Messages(ctxT(t), "bot/b")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"second", "first\n\nwith a body\nof two lines"}; !slices.Equal(got, want) {
		t.Errorf("Messages = %q; want %q", got, want)
	}
	if none, err := f.r.Messages(ctxT(t), "main"); err != nil || len(none) != 0 {
		t.Errorf("Messages(main) = %q, %v", none, err)
	}
}

// What a branch keeps: the changed paths without the deleted ones, which
// are public already.
func TestKeptFilesSkipsDeletions(t *testing.T) {
	f := newFixture(t)
	f.repo.Git("checkout", "-q", "-b", "bot/b")
	f.repo.Git("rm", "-q", "lib/thing.rb")
	f.repo.Write("README.md", "changed\n")
	f.repo.Write("lib/new.rb", "x = 1\n")
	f.repo.Commit("change")
	f.repo.Git("checkout", "-q", "main")
	got, err := f.r.KeptFiles(ctxT(t), "bot/b")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"README.md", "lib/new.rb"}; !slices.Equal(got, want) {
		t.Errorf("KeptFiles = %q; want %q", got, want)
	}
	if all, _ := f.r.ChangedFiles(ctxT(t), "bot/b"); !slices.Contains(all, "lib/thing.rb") {
		t.Errorf("ChangedFiles = %q: the deletion is a change", all)
	}
}

// The push publishes every commit, so a path one commit adds and a later
// one removes is still kept.
func TestKeptFilesKeepsEachCommitsPaths(t *testing.T) {
	f := newFixture(t)
	f.repo.Git("checkout", "-q", "-b", "bot/b")
	f.repo.Write("notes/hostx.txt", "x\n")
	f.repo.Commit("add")
	f.repo.Git("rm", "-q", "notes/hostx.txt")
	f.repo.Write("README.md", "changed\n")
	f.repo.Commit("remove")
	f.repo.Git("checkout", "-q", "main")
	got, err := f.r.KeptFiles(ctxT(t), "bot/b")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"README.md", "notes/hostx.txt"}; !slices.Equal(got, want) {
		t.Errorf("KeptFiles = %q; want %q", got, want)
	}
}

// The added lines a branch publishes: every commit's, so a line one commit
// adds and a later one removes is there; as text whatever git calls
// binary (a NUL byte, or -diff in the branch's own attributes); a line
// whose text starts with "++" is a line, not a header; and a carriage
// return inside a line does not cut it. Removed lines are not.
func TestPublishedLinesReadsEachCommitAsText(t *testing.T) {
	f := newFixture(t)
	f.repo.Git("checkout", "-q", "-b", "bot/b")
	f.repo.Write(".gitattributes", "* -diff\n")
	f.repo.Write("a.txt", "keep\n10.9.8.7\n++ hostx\nok\rhostx\n")
	f.repo.Write("blob.dat", "bin\x00hostx\n")
	f.repo.Commit("add")
	f.repo.Write("a.txt", "keep\n")
	f.repo.Commit("take it out")
	f.repo.Git("checkout", "-q", "main")
	got, err := f.r.PublishedLines(ctxT(t), "bot/b")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"* -diff", "keep", "10.9.8.7", "++ hostx", "ok\rhostx", "bin\x00hostx"} {
		if !slices.Contains(got, want) {
			t.Errorf("PublishedLines = %q; lacks %q", got, want)
		}
	}
	if n := len(got); n != 6 {
		t.Errorf("PublishedLines = %q: want each line once", got)
	}
	if none, err := f.r.PublishedLines(ctxT(t), "main"); err != nil || len(none) != 0 {
		t.Errorf("PublishedLines(main) = %q, %v", none, err)
	}
}

// An added line whose text starts with "++" (C's ++i;) is an added line.
func TestAddedLinesKeepsALineStartingPlusPlus(t *testing.T) {
	f := newFixture(t)
	f.repo.BranchWith("bot/b", map[string]string{"loop.c": "++i;\n"})
	got, err := f.r.AddedLines(ctxT(t), "bot/b")
	if err != nil || !slices.Equal(got, []string{"++i;"}) {
		t.Errorf("AddedLines = %q, %v", got, err)
	}
}

// A rename is touched at both ends: the old path and the new.
func TestTouchedFilesListsBothEndsOfARename(t *testing.T) {
	f := newFixture(t)
	f.repo.Git("checkout", "-q", "-b", "bot/b")
	f.repo.Git("mv", "lib/thing.rb", "x.md")
	f.repo.Commit("move")
	f.repo.Git("checkout", "-q", "main")
	got, err := f.r.TouchedFiles(ctxT(t), "bot/b")
	if err != nil || !slices.Equal(got, []string{"lib/thing.rb", "x.md"}) {
		t.Errorf("TouchedFiles = %q, %v", got, err)
	}
	if changed, _ := f.r.ChangedFiles(ctxT(t), "bot/b"); !slices.Equal(changed, []string{"x.md"}) {
		t.Errorf("ChangedFiles = %q: unchanged, the new path only", changed)
	}
}
