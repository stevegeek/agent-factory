package memory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/memstore"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Closed is the set of tickets the monthly report counts as closed: done
// or rejected (memory_report; a dropped ticket is not among them).
func Closed(ts []*ticket.Ticket) map[string]bool {
	out := map[string]bool{}
	for _, t := range ts {
		if t.Column == "done" || t.Column == "rejected" {
			out[string(t.ID)] = true
		}
	}
	return out
}

// Report is memory_report: each store's file count and size,
// flagged when over [memory] max_kb, and its notes for closed tickets,
// gathered without a model, for the clock's monthly notice. ok is false
// when there is no store. A store git cannot read is passed over.
func (s *Service) Report(ctx context.Context, cfg *config.Config, closed map[string]bool) (string, bool) {
	stores, err := s.stores("", "")
	if err != nil {
		return "", false
	}
	limit := cfg.Memory.MaxKB
	var lines []string
	staleTotal := 0
	for _, st := range stores {
		repo, err := s.Store.Repo(st.project, st.role)
		if err != nil {
			continue
		}
		head, err := repo.Head(ctx)
		if err != nil {
			continue
		}
		files, err := repo.Snapshot(ctx, head)
		if err != nil {
			continue
		}
		size := memstore.Size(files)
		var stale []string
		for p := range files {
			if strings.HasPrefix(p, "tickets/") && closed[noteID(p)] {
				stale = append(stale, p)
			}
		}
		slices.Sort(stale)
		staleTotal += len(stale)
		line := "- **" + st.project + "/" + st.role + "**: " + strconv.Itoa(len(files)) + " files, " + strconv.Itoa(size/1024) + " KB"
		if size > limit*1024 {
			line += " **over its limit**"
		}
		if len(stale) > 0 {
			ids := make([]string, 0, 12)
			for _, p := range stale[:min(12, len(stale))] {
				ids = append(ids, noteID(p))
			}
			line += "; notes for closed tickets: " + strings.Join(ids, ", ")
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "", false
	}
	out := "The agents' memory stores (`factory memory <project> <role>` shows each one's history):\n\n" +
		strings.Join(lines, "\n") + "\n\n"
	if staleTotal > 0 {
		out += strconv.Itoa(staleTotal) + " ticket notes belong to closed tickets; they can go when nothing in them is still " +
			"useful (a `factory memory --revert` undoes any change). "
	}
	return out + "Nothing has been changed.", true
}

// noteID is p[8:-3] in code points: tickets/T0003.md -> T0003.
func noteID(p string) string {
	r := []rune(p)
	if len(r)-3 <= 8 {
		return ""
	}
	return string(r[8 : len(r)-3])
}

// Listing is `factory memory [<project> [<role>]]`: for each
// matching store, "== project/role" and its last 20 commits with their
// file stats. project and role are glob patterns, empty meaning any; a
// pattern naming a path ("a/b", "..") matches nothing.
func (s *Service) Listing(ctx context.Context, w io.Writer, project, role string) error {
	stores, err := s.stores(project, role)
	if err != nil {
		return err
	}
	for _, st := range stores {
		if _, err := fmt.Fprintf(w, "== %s/%s\n", st.project, st.role); err != nil {
			return err
		}
		repo, err := s.Store.Repo(st.project, st.role)
		if err != nil {
			return err
		}
		log, err := repo.Log(ctx)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, log); err != nil {
			return err
		}
	}
	return nil
}

// Revert is `factory memory P R --revert COMMIT`: one change
// undone, and the line it prints. It needs the project and the role; a
// store that does not exist is a clean refusal.
func (s *Service) Revert(ctx context.Context, project, role, commit string) (string, error) {
	if project == "" || role == "" {
		return "", errors.New("--revert needs the project and the role")
	}
	repo, err := s.Store.Open(project, role)
	if err != nil {
		return "", err
	}
	if err := repo.Revert(ctx, commit); err != nil {
		return "", err
	}
	return "undid " + commit + " in " + project + "/" + role, nil
}

type storeName struct{ project, role string }

// stores is sorted(glob(memory/<project or *>/<role or *>.git)) with
// Python's glob rules: fnmatch patterns, and a wildcard never matching a
// name that starts with "." unless the pattern does. Only stores whose
// names a store can have are listed.
func (s *Service) stores(project, role string) ([]storeName, error) {
	if project == "" {
		project = "*"
	}
	if role == "" {
		role = "*"
	}
	role += ".git"
	for _, p := range []string{project, role} {
		if strings.Contains(p, "/") || p == "." || p == ".." {
			return nil, nil
		}
	}
	var out []storeName
	projects, err := globIn(s.Paths.Memory, project, true)
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		roles, err := globIn(filepath.Join(s.Paths.Memory, p), role, false)
		if err != nil {
			return nil, err
		}
		for _, r := range roles {
			name := strings.TrimSuffix(r, ".git")
			if memstore.KeyOK(p) && memstore.KeyOK(name) {
				out = append(out, storeName{p, name})
			}
		}
	}
	slices.SortFunc(out, func(a, b storeName) int {
		return strings.Compare(a.project+"/"+a.role+".git", b.project+"/"+b.role+".git")
	})
	return out, nil
}

// globIn is one level of glob in dir: the names matching pattern (only
// directories, links to them included, when dirOnly).
func globIn(dir, pattern string, dirOnly bool) ([]string, error) {
	isDir := func(name string) bool {
		st, err := os.Stat(filepath.Join(dir, name))
		return err == nil && st.IsDir()
	}
	if !strings.ContainsAny(pattern, "*?[") {
		if _, err := os.Lstat(filepath.Join(dir, pattern)); err != nil || dirOnly && !isDir(pattern) {
			return nil, nil
		}
		return []string{pattern}, nil
	}
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("memory stores: %w", err)
	}
	var out []string
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(pattern, ".") {
			continue
		}
		if dirOnly && !isDir(name) {
			continue
		}
		if textx.Fnmatch(name, pattern) {
			out = append(out, name)
		}
	}
	return out, nil
}
