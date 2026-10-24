// Package builds keeps the dispatcher's build candidates: a core commit's
// three binaries and BUILD, built on a builder host (factory-remote build)
// and checked here before they are stored in ~/factory/builds/<commit12>/,
// where deploy.sh's candidate verb installs them from. A candidate only
// ever appears whole: it is written to a temp dir and renamed into place.
package builds

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
)

// Keep is how many candidates stay: the newest by mtime.
const Keep = 5

// lockWait bounds waiting for another Put on the same store.
const lockWait = 2 * time.Minute

// Has reports a stored candidate for commit.
func Has(dir, commit string) bool {
	if !jobproto.Commit12OK(commit) {
		return false
	}
	fi, err := os.Lstat(filepath.Join(dir, commit, "BUILD"))
	return err == nil && fi.Mode().IsRegular()
}

// Put checks a builder's answer for commit, built with goVersion, and
// stores it as the candidate dir/<commit>: exactly the three binaries
// (mode 0755) and BUILD, each binary's sha256 the one BUILD gives, BUILD's
// last line "commit <commit> go <goVersion>", plus HOST naming the
// builder. An existing candidate is kept as it is. Then only the newest
// Keep candidates stay.
func Put(dir, commit, goVersion, host string, answer []byte) error {
	if !jobproto.Commit12OK(commit) {
		return fmt.Errorf("bad commit %q", commit)
	}
	files, err := jobproto.UntarFlat(answer)
	if err != nil {
		return fmt.Errorf("the build's answer: %w", err)
	}
	if err := check(files, commit, goVersion); err != nil {
		return fmt.Errorf("the build of %s: %w", commit, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), lockWait)
	defer cancel()
	lock, err := fsx.Lock(ctx, filepath.Join(dir, ".lock"), fsx.Wait(lockWait))
	if err != nil {
		return err
	}
	defer lock.Unlock()
	if err := clearTemps(dir); err != nil {
		return err
	}
	if Has(dir, commit) {
		return nil
	}
	tmp := filepath.Join(dir, ".tmp-"+commit+"-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.Mkdir(tmp, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(tmp) // gone after the rename; cleans up a failure
	for _, n := range jobproto.BuildFiles() {
		mode := os.FileMode(0o755)
		if n == "BUILD" {
			mode = 0o644
		}
		if err := os.WriteFile(filepath.Join(tmp, n), files[n], mode); err != nil {
			return err
		}
		if err := os.Chmod(filepath.Join(tmp, n), mode); err != nil { // past the umask
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, "HOST"), []byte(host+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, commit)); err != nil {
		return err
	}
	return prune(dir)
}

// check is the answer's own consistency: the four files and nothing else,
// each binary's sha256 as BUILD lists it, BUILD naming commit and goVersion.
func check(files map[string][]byte, commit, goVersion string) error {
	var names []string
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	want := slices.Clone(jobproto.BuildFiles())
	slices.Sort(want)
	if !slices.Equal(names, want) {
		return fmt.Errorf("it holds %v, not %v", names, want)
	}
	lines := strings.Split(strings.TrimSuffix(string(files["BUILD"]), "\n"), "\n")
	if len(lines) != 4 {
		return fmt.Errorf("BUILD has %d lines, not 4", len(lines))
	}
	if last := "commit " + commit + " go " + goVersion; lines[3] != last {
		return fmt.Errorf("BUILD says %q, not %q", lines[3], last)
	}
	for i, n := range jobproto.BuildFiles()[:3] {
		sum := sha256.Sum256(files[n])
		if lines[i] != hex.EncodeToString(sum[:])+"  "+n {
			return fmt.Errorf("%s does not match its BUILD line", n)
		}
	}
	return nil
}

// clearTemps removes what a Put that died left behind.
func clearTemps(dir string) error {
	es, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range es {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// prune keeps the newest Keep candidate dirs by mtime.
func prune(dir string) error {
	es, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type cand struct {
		name string
		at   time.Time
	}
	var cs []cand
	for _, e := range es {
		if !e.IsDir() || !jobproto.Commit12OK(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return err
		}
		cs = append(cs, cand{e.Name(), fi.ModTime()})
	}
	slices.SortFunc(cs, func(a, b cand) int { return b.at.Compare(a.at) })
	var errs []error
	for _, c := range cs[min(Keep, len(cs)):] {
		errs = append(errs, os.RemoveAll(filepath.Join(dir, c.name)))
	}
	return errors.Join(errs...)
}

// SourceTar is the tar `factory build` sends a builder: the tree at dir,
// its directories and regular files only (a link or anything else refuses
// it), an executable file kept executable, at most MaxBuildInput; and the
// Go its go.mod pins.
func SourceTar(dir string) ([]byte, string, error) {
	gomod, err := fsx.ReadNoFollow(filepath.Join(dir, "go.mod"), 64<<10)
	if err != nil {
		return nil, "", fmt.Errorf("the source's go.mod: %w", err)
	}
	pin, err := jobproto.ToolchainPin(gomod)
	if err != nil {
		return nil, "", err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: rel + "/", Mode: 0o755})
		case d.Type().IsRegular():
			fi, err := d.Info()
			if err != nil {
				return err
			}
			mode := int64(0o644)
			if fi.Mode().Perm()&0o100 != 0 {
				mode = 0o755
			}
			b, err := fsx.ReadNoFollow(path, jobproto.MaxBuildInput)
			if err != nil {
				return err
			}
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: rel, Mode: mode, Size: int64(len(b))}); err != nil {
				return err
			}
			if _, err := tw.Write(b); err != nil {
				return err
			}
			if buf.Len() > jobproto.MaxBuildInput {
				return fmt.Errorf("the source is over %d bytes", jobproto.MaxBuildInput)
			}
			return nil
		}
		return fmt.Errorf("%s is not a file or a directory", rel)
	})
	if err != nil {
		return nil, "", err
	}
	if err := tw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), pin, nil
}
