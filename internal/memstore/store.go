package memstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/gitx"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// ErrNoStore is returned by Open for a (project, role) with no store yet.
var ErrNoStore = errors.New("no memory store")

// ErrRootCommit is Revert's refusal of a store's first commit.
var ErrRootCommit = errors.New("that is the store's first commit; there is nothing before it to go back to")

// ConflictError is Revert's refusal of a change later ones built on.
type ConflictError struct{ Commit string }

func (e *ConflictError) Error() string {
	return "undoing " + e.Commit + " conflicts with later changes; nothing changed"
}

// The first commit's files (MEMORY_README and its folders, F:1002).
const readme = `# Memory: {role} on {project}

What agents in this role on this project have learned, kept between runs. Plain Markdown; every
change is a git commit made by the factory after each run, so it can be read and undone.

- ` + "`knowledge/INDEX.md`" + `: one line per topic file in ` + "`knowledge/`" + `. Keep it short.
- ` + "`knowledge/<topic>.md`" + `: durable facts and rules, one topic per file.
- ` + "`tickets/<ticket>.md`" + `: per ticket: what was done, where it stopped, what's next.
`

const mainRef = "refs/heads/main"

// Store is the memory directory: memory/<project>/<role>.git.
type Store struct {
	Paths paths.Paths
	Exec  execx.Execer // nil runs git for real (execx.OS)
	Env   []string     // the base environment of git; nil is this process's
}

// Repo is one store.
type Repo struct {
	Project, Role string
	GitDir        string
	git           gitx.Bare
}

func (s *Store) execer() execx.Execer {
	if s.Exec == nil {
		return execx.OS{}
	}
	return s.Exec
}

// Repo is the store of (project, role), which may not exist yet. Names
// that are not plain are refused (memory_repo's ValueError).
func (s *Store) Repo(project, role string) (*Repo, error) {
	if !KeyOK(project) || !KeyOK(role) {
		return nil, nameError(project, role)
	}
	return s.at(project, role, s.Paths.MemoryRepo(project, role)), nil
}

func (s *Store) at(project, role, gitDir string) *Repo {
	return &Repo{Project: project, Role: role, GitDir: gitDir,
		git: gitx.Bare{GitDir: gitDir, Exec: s.execer(), Env: s.Env}}
}

// Open is the existing store of (project, role): ErrNoStore when there is
// none (a clean refusal where Python printed a traceback).
func (s *Store) Open(project, role string) (*Repo, error) {
	r, err := s.Repo(project, role)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(r.GitDir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("%w for %s/%s", ErrNoStore, project, role)
	}
	return r, nil
}

// Ensure is ensure_memory (F:1135): the store, created on first use in
// <repo>.new (bare, branch main, no template, so no sample hooks) with a
// README and empty folders, then renamed into place, so no half-made store
// is ever seen.
func (s *Store) Ensure(ctx context.Context, project, role string) (*Repo, error) {
	r, err := s.Repo(project, role)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(r.GitDir); err == nil && st.IsDir() {
		return r, nil
	}
	if err := os.MkdirAll(filepath.Dir(r.GitDir), 0o777); err != nil {
		return nil, fmt.Errorf("memory store %s/%s: %w", project, role, err)
	}
	tmp := r.GitDir + ".new"
	if err := fsx.RemoveTree(tmp); err != nil {
		return nil, fmt.Errorf("memory store %s/%s: %w", project, role, err)
	}
	if err := s.create(ctx, s.at(project, role, tmp)); err != nil {
		_ = fsx.RemoveTree(tmp)
		return nil, err
	}
	if err := os.Rename(tmp, r.GitDir); err != nil {
		_ = fsx.RemoveTree(tmp)
		return nil, fmt.Errorf("memory store %s/%s: %w", project, role, err)
	}
	return r, nil
}

func (s *Store) create(ctx context.Context, t *Repo) error {
	if err := os.Mkdir(t.GitDir, 0o777); err != nil {
		return fmt.Errorf("memory store %s/%s: %w", t.Project, t.Role, err)
	}
	if _, err := t.git.Run(ctx, bareCmd("init", "-q", "--bare", "-b", "main", "--template=")); err != nil {
		return err
	}
	text := strings.NewReplacer("{role}", t.Role, "{project}", t.Project).Replace(readme)
	tree, err := t.WriteTree(ctx, Files{
		"README.md":          []byte(text),
		"knowledge/INDEX.md": []byte("# Knowledge index\n\n(nothing yet)\n"),
		"tickets/.keep":      {},
	}, nil)
	if err != nil {
		return err
	}
	c, err := t.CommitTree(ctx, tree, nil, "memory for "+t.Role+" on "+t.Project)
	if err != nil {
		return err
	}
	_, err = t.git.Run(ctx, bareCmd("update-ref", mainRef, c))
	return err
}

func bareCmd(args ...string) gitx.BareCmd { return gitx.BareCmd{Args: args} }

func (r *Repo) out(ctx context.Context, args ...string) (string, error) {
	return r.git.Out(ctx, bareCmd(args...))
}

// Head is the store's latest commit (memory_head).
func (r *Repo) Head(ctx context.Context) (string, error) { return r.out(ctx, "rev-parse", mainRef) }

// TreeOf is the tree of a commit.
func (r *Repo) TreeOf(ctx context.Context, rev string) (string, error) {
	return r.out(ctx, "rev-parse", rev+"^{tree}")
}

// Short is the abbreviated name of a commit.
func (r *Repo) Short(ctx context.Context, rev string) (string, error) {
	return r.out(ctx, "rev-parse", "--short", rev)
}

// Listing is memory_listing: {path: blob} of a commit, regular non-executable
// blobs only (no links, executables or submodules).
func (r *Repo) Listing(ctx context.Context, rev string) (map[string]string, error) {
	res, err := r.git.Run(ctx, bareCmd("ls-tree", "-r", "-z", rev))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, entry := range bytes.Split(res.Stdout, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		meta, path, ok := bytes.Cut(entry, []byte{'\t'})
		if !ok {
			return nil, fmt.Errorf("memory git ls-tree: odd entry %q", entry)
		}
		f := strings.Fields(string(meta))
		if len(f) != 3 {
			return nil, fmt.Errorf("memory git ls-tree: odd entry %q", entry)
		}
		if f[1] == "blob" && f[0] == "100644" {
			out[decode(path)] = f[2]
		}
	}
	return out, nil
}

// Snapshot is memory_snapshot: {path: contents} of a commit, read from git
// objects with one cat-file --batch, never from a directory.
func (r *Repo) Snapshot(ctx context.Context, rev string) (Files, error) {
	listing, err := r.Listing(ctx, rev)
	if err != nil {
		return nil, err
	}
	files := Files{}
	if len(listing) == 0 {
		return files, nil
	}
	ps := make([]string, 0, len(listing))
	for p := range listing {
		ps = append(ps, p)
	}
	sort.Strings(ps)
	shas := make([]string, len(ps))
	for i, p := range ps {
		shas[i] = listing[p]
	}
	data, err := r.git.CatFileBatch(ctx, shas)
	if err != nil {
		return nil, err
	}
	for i, p := range ps {
		files[p] = data[i]
	}
	return files, nil
}

// WriteTree is memory_tree: a tree of files, every one mode 100644, built
// through a private index (no work tree, filters or attributes). A blob
// is written only when known (the base listing) does not already name it.
func (r *Repo) WriteTree(ctx context.Context, files Files, known map[string]string) (string, error) {
	var info bytes.Buffer
	for _, p := range sortedPaths(files) {
		data := files[p]
		sha := BlobSHA(data)
		if known[p] != sha {
			c := bareCmd("hash-object", "-w", "--no-filters", "--stdin")
			c.Stdin = data
			if c.Stdin == nil {
				c.Stdin = []byte{}
			}
			var err error
			if sha, err = r.git.Out(ctx, c); err != nil {
				return "", err
			}
		}
		info.WriteString("100644 " + sha + "\t" + p + "\x00")
	}
	dir, err := os.MkdirTemp("", "factory-memory-index-")
	if err != nil {
		return "", fmt.Errorf("memory index: %w", err)
	}
	defer os.RemoveAll(dir)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index")}
	if info.Len() > 0 {
		c := bareCmd("update-index", "--add", "-z", "--index-info")
		c.Stdin, c.Env = info.Bytes(), env
		if _, err := r.git.Run(ctx, c); err != nil {
			return "", err
		}
	}
	c := bareCmd("write-tree")
	c.Env = env
	return r.git.Out(ctx, c)
}

// CommitTree is memory_commit: a commit of tree with the given parents.
func (r *Repo) CommitTree(ctx context.Context, tree string, parents []string, message string) (string, error) {
	args := []string{"commit-tree", tree, "-m", message}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	return r.out(ctx, args...)
}

// Changed lists the paths that differ between two commits (no renames).
func (r *Repo) Changed(ctx context.Context, from, to string) ([]string, error) {
	res, err := r.git.Run(ctx, bareCmd("diff-tree", "-r", "--name-only", "-z", "--no-renames", from, to))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range bytes.Split(res.Stdout, []byte{0}) {
		if len(p) > 0 {
			out = append(out, decode(p))
		}
	}
	return out, nil
}

// UpdateRef moves main to newHead if it is still at old.
func (r *Repo) UpdateRef(ctx context.Context, newHead, old string) error {
	_, err := r.git.Run(ctx, bareCmd("update-ref", mainRef, newHead, old))
	return err
}

// Lock takes the store's flock, <repo>.lock, waiting as long as ctx allows
// (memory_lock).
func (r *Repo) Lock(ctx context.Context) (*fsx.Locked, error) {
	return fsx.Lock(ctx, r.GitDir+".lock", fsx.Forever)
}

// mergeTree is merge-tree --write-tree --merge-base=<base> ours theirs: the
// merged tree, and whether it holds conflicts (kept in the files).
func (r *Repo) mergeTree(ctx context.Context, base, ours, theirs string) (string, bool, error) {
	c := bareCmd("merge-tree", "--write-tree", "--merge-base="+base, ours, theirs)
	c.OK = []int{0, 1}
	res, err := r.git.Run(ctx, c)
	if err != nil {
		return "", false, err
	}
	first, _, _ := strings.Cut(decode(res.Stdout), "\n")
	return strings.TrimSpace(first), res.Code == 1, nil
}

// Land puts a run's commit (made on base) into the store under its lock
// (F:1351): if the head has moved since base, the two are merged with
// merge-tree, both sides of a conflict kept in the file, as a merge commit
// "<message> (merged)" or "<message> (merged; both sides kept)". The ref
// moves only if the head is still the one merged. It returns the new head.
func (r *Repo) Land(ctx context.Context, base, run, message string) (head string, conflicted bool, err error) {
	l, err := r.Lock(ctx)
	if err != nil {
		return "", false, fmt.Errorf("memory lock: %w", err)
	}
	defer l.Unlock()
	old, err := r.Head(ctx)
	if err != nil {
		return "", false, err
	}
	head = run
	if old != base {
		tree, c, err := r.mergeTree(ctx, base, old, run)
		if err != nil {
			return "", false, err
		}
		conflicted = c
		suffix := " (merged)"
		if c {
			suffix = " (merged; both sides kept)"
		}
		if head, err = r.CommitTree(ctx, tree, []string{old, run}, message+suffix); err != nil {
			return "", false, err
		}
	}
	if err := r.UpdateRef(ctx, head, old); err != nil {
		return "", false, err
	}
	return head, conflicted, nil
}

// Revert undoes one commit with a three-way merge onto the head (factory
// memory --revert, F:1414), committed as "undo <commit[:12]>". The first
// commit and a change later ones built on are refused, and nothing changes.
func (r *Repo) Revert(ctx context.Context, commit string) error {
	l, err := r.Lock(ctx)
	if err != nil {
		return fmt.Errorf("memory lock: %w", err)
	}
	defer l.Unlock()
	head, err := r.Head(ctx)
	if err != nil {
		return err
	}
	c, err := r.out(ctx, "rev-parse", "--verify", commit+"^{commit}")
	if err != nil {
		return err
	}
	parent := bareCmd("rev-parse", "--verify", "-q", c+"^")
	parent.OK = []int{0, 1}
	res, err := r.git.Run(ctx, parent)
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return ErrRootCommit
	}
	tree, conflicted, err := r.mergeTree(ctx, c, head, c+"^")
	if err != nil {
		return err
	}
	if conflicted {
		return &ConflictError{Commit: commit}
	}
	undo, err := r.CommitTree(ctx, tree, []string{head}, "undo "+textx.Prefix(commit, 12))
	if err != nil {
		return err
	}
	return r.UpdateRef(ctx, undo, head)
}

// Log is the store's history as `factory memory` prints it: the last 20
// commits with their file stats.
func (r *Repo) Log(ctx context.Context) (string, error) {
	return r.out(ctx, "log", "--stat", "--format=%h %ad %s", "--date=short", "-n", "20", mainRef)
}
