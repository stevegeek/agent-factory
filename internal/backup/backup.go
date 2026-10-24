// Package backup is `factory backup` (ARCHITECTURE.md §15.2): a daily
// encrypted archive of the box's state to Cloudflare R2, read back after
// each upload, with retention, a weekly restore check, and restore into an
// empty directory.
//
// The archive holds board/, memory/ (the bare stores, as files), context/,
// config.toml, the token store (hashes only), foreman-state/ (without the
// login secret or sessions; with Foreman's foreman.log and its rotated
// files), state.json, pauses.json, watches.json,
// images.json, logs/ and repos/ (each project's unpushed bot/ branches as a
// bundle, and its pinned untracked files). It never holds a credential: the Claude token,
// the App key, the Anthropic key, ssh keys, Foreman's secret, the backup
// key or the R2 keys. Foreman's push key goes in only with
// include_push_key (default true). board/ and memory/ are copied under the
// tick lock, so no ticket or memory store is torn mid-save; the rest is
// copied after the lock is let go.
package backup

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// TickLocker takes the tick's lock (store.Board does).
type TickLocker interface {
	TickLock(ctx context.Context, wait fsx.Wait) (*fsx.Locked, error)
}

// Noticer files a notice for the maintainer (doors.Service does).
type Noticer interface {
	Notice(ctx context.Context, title, body string) error
}

// Bucket is the object store (S3 is the R2 one; tests pass a fake server
// through S3 too).
type Bucket interface {
	PutFile(ctx context.Context, key, path, sum string) error
	PutBytes(ctx context.Context, key string, body []byte) error
	Get(ctx context.Context, key string, w io.Writer) (int64, error)
	List(ctx context.Context, prefix string) ([]Object, error)
}

// deleter is the Bucket's DELETE, kept apart so that every interface here
// stays small.
type deleter interface {
	Delete(ctx context.Context, key string) error
}

// Logger is the factory log.
type Logger interface {
	Printf(format string, args ...any)
}

// Service runs backups for one host.
type Service struct {
	Paths   paths.Paths
	Lock    TickLocker
	Bucket  Bucket // nil: built from [backup] by Open
	Notices Noticer
	Clock   clock.Clock
	Log     Logger
	Host    string    // in the archive's name; "" is the machine's host name
	Random  io.Reader // nil: crypto/rand
	Chunk   int       // plaintext per sealed chunk; 0 is DefaultChunk
	// Repos writes the archive's repos/: what the projects' clones hold
	// that GitHub does not. nil leaves it out.
	Repos RepoSaver
	// afterLocked runs between the locked copy and the rest (tests check
	// that the lock is free by then).
	afterLocked func()
}

// RepoSaver writes into dir what only the projects' clones hold (the CLI's
// saves unpushed bot/ branches as bundles, and the pinned untracked files);
// the clones themselves come back from GitHub.
type RepoSaver interface {
	Save(ctx context.Context, dir string) error
}

// Open builds the R2 bucket from [backup] and its credentials file.
func Open(cfg config.Backup, p paths.Paths) (*S3, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errNotConfigured
	}
	credPath := cfg.Credentials
	if credPath == "" {
		credPath = p.BackupCredentials()
	}
	creds, err := ReadCredentials(credPath)
	if err != nil {
		return nil, err
	}
	return &S3{Endpoint: cfg.Endpoint, Bucket: cfg.Bucket, Region: cfg.Region, Creds: creds}, nil
}

// ReadCredentials reads the R2 key pair: KEY=value lines (or KEY = "value")
// naming access_key_id and secret_access_key (or their AWS_ forms). The
// file must be readable by its owner only.
func ReadCredentials(path string) (Credentials, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Credentials{}, fmt.Errorf("backup credentials: %w", err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return Credentials{}, fmt.Errorf("backup credentials: %s is mode %#o; it must be 0600", path, fi.Mode().Perm())
	}
	f, err := os.Open(path)
	if err != nil {
		return Credentials{}, fmt.Errorf("backup credentials: %w", err)
	}
	defer f.Close()
	var c Credentials
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "access_key_id", "aws_access_key_id":
			c.AccessKeyID = v
		case "secret_access_key", "aws_secret_access_key":
			c.SecretAccessKey = v
		}
	}
	if err := sc.Err(); err != nil {
		return Credentials{}, fmt.Errorf("backup credentials: %w", err)
	}
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return Credentials{}, fmt.Errorf("backup credentials: %s needs access_key_id and secret_access_key", path)
	}
	return c, nil
}

// keyPath is the backup key's file.
func keyPath(cfg config.Backup, p paths.Paths) string {
	if cfg.KeyFile != "" {
		return cfg.KeyFile
	}
	return p.BackupKey()
}

func (s Service) host() string {
	h := s.Host
	if h == "" {
		h, _ = os.Hostname()
	}
	if h = textx.Slugify(h); h == "" || h == "change" {
		h = "host"
	}
	return h
}

func (s Service) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log.Printf(format, args...)
	}
}

func (s Service) retention(cfg config.Backup) Retention {
	r := Retention{Daily: cfg.Daily, Weekly: cfg.Weekly, Monthly: cfg.Monthly}
	if r == (Retention{}) {
		r = DefaultRetention()
	}
	return r
}

// Result is what one backup made.
type Result struct {
	Key     string // the archive's object key
	Size    int64
	SHA256  string
	Deleted []string // archives retention removed
}

// Run is `factory backup`: stage, archive, encrypt, upload, read back, and
// only then apply retention. A failure files a notice and is returned.
func (s Service) Run(ctx context.Context, cfg config.Backup) (Result, error) {
	res, err := s.run(ctx, cfg)
	if err != nil {
		s.logf("backup failed: %v", err)
		s.notice(ctx, "Daily backup failed", fmt.Sprintf("`factory backup` failed: %v\n\nThe previous backups are untouched.", err))
		return res, err
	}
	s.logf("backup: %s (%d bytes) uploaded and read back", res.Key, res.Size)
	return res, nil
}

func (s Service) notice(ctx context.Context, title, body string) {
	if s.Notices == nil {
		return
	}
	if err := s.Notices.Notice(context.WithoutCancel(ctx), title, body); err != nil {
		s.logf("backup: could not file the notice %q: %v", title, err)
	}
}

func (s Service) run(ctx context.Context, cfg config.Backup) (Result, error) {
	key, err := ReadKey(keyPath(cfg, s.Paths))
	if err != nil {
		return Result{}, err
	}
	bucket := s.Bucket
	if bucket == nil {
		b, err := Open(cfg, s.Paths)
		if err != nil {
			return Result{}, err
		}
		bucket = b
	}
	work, err := s.workDir("backup-")
	if err != nil {
		return Result{}, err
	}
	defer fsx.RemoveTree(work)
	staged := filepath.Join(work, "staged")
	if err := s.Stage(ctx, staged, cfg.IncludePushKey); err != nil {
		return Result{}, err
	}
	now := s.Clock.Now()
	name := archiveName(s.host(), now)
	out := filepath.Join(work, name)
	if err := s.seal(ctx, staged, out, key, now); err != nil {
		return Result{}, err
	}
	// the staged plaintext goes as soon as it is sealed
	if err := fsx.RemoveTree(staged); err != nil {
		return Result{}, fmt.Errorf("backup: %w", err)
	}
	sum, size, err := fileSHA256(out)
	if err != nil {
		return Result{}, fmt.Errorf("backup: %w", err)
	}
	objKey := cfg.Prefix + name
	res := Result{Key: objKey, Size: size, SHA256: sum}
	if err := bucket.PutFile(ctx, objKey, out, sum); err != nil {
		return res, err
	}
	if err := bucket.PutBytes(ctx, objKey+sidecarSuffix, []byte(sidecar(sum, size))); err != nil {
		return res, err
	}
	if err := readBack(ctx, bucket, objKey, sum, size); err != nil {
		return res, err
	}
	res.Deleted, err = s.prune(ctx, bucket, cfg)
	return res, err
}

func sidecar(sum string, size int64) string {
	return sum + " " + strconv.FormatInt(size, 10) + "\n"
}

func parseSidecar(b []byte) (string, int64, bool) {
	f := strings.Fields(string(b))
	if len(f) != 2 || len(f[0]) != 64 {
		return "", 0, false
	}
	n, err := strconv.ParseInt(f[1], 10, 64)
	return f[0], n, err == nil
}

// readBack downloads the object again and checks its size and SHA-256.
func readBack(ctx context.Context, b Bucket, key, sum string, size int64) error {
	h := sha256.New()
	n, err := b.Get(ctx, key, h)
	if err != nil {
		return fmt.Errorf("backup: reading %s back: %w", key, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); n != size || got != sum {
		return fmt.Errorf("backup: %s read back as %d bytes, sha256 %s; uploaded %d bytes, sha256 %s", key, n, got, size, sum)
	}
	return nil
}

// workDir is a private directory under the root's run/ (never /tmp: the
// staged plaintext stays on the factory's own disk, mode 0700).
func (s Service) workDir(pattern string) (string, error) {
	base := s.Paths.Run
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("backup: %w", err)
	}
	d, err := os.MkdirTemp(base, pattern)
	if err != nil {
		return "", fmt.Errorf("backup: %w", err)
	}
	return d, nil
}

// Stage copies what the archive holds into dir: board/ and memory/ under
// the tick lock (waiting for a running tick, bounded by ctx), the rest
// after it is let go.
func (s Service) Stage(ctx context.Context, dir string, pushKey bool) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	l, err := s.Lock.TickLock(ctx, store.Forever)
	if err != nil {
		return fmt.Errorf("backup: tick lock: %w", err)
	}
	err = stage(ctx, lockedItems(s.Paths), dir, pushKey)
	l.Unlock()
	if err != nil {
		return err
	}
	if s.afterLocked != nil {
		s.afterLocked()
	}
	if err := stage(ctx, otherItems(s.Paths), dir, pushKey); err != nil {
		return err
	}
	if s.Repos == nil {
		return nil
	}
	if err := s.Repos.Save(ctx, filepath.Join(dir, "repos")); err != nil {
		return fmt.Errorf("backup: repos: %w", err)
	}
	return nil
}

// seal writes dir as an encrypted tar.gz into out.
func (s Service) seal(ctx context.Context, dir, out string, key []byte, now time.Time) error {
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	chunk := s.Chunk
	if chunk == 0 {
		chunk = DefaultChunk
	}
	enc, err := Encrypt(f, key, chunk, cryptoRand(s.Random))
	if err != nil {
		f.Close()
		return err
	}
	if err := writeTar(ctx, enc, dir, now.UTC().Truncate(time.Second)); err != nil {
		f.Close()
		return err
	}
	if err := enc.Close(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("backup: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	return nil
}

// archives are this host's archives in the bucket, newest first.
func (s Service) archives(ctx context.Context, b Bucket, cfg config.Backup) ([]Object, error) {
	objs, err := b.List(ctx, cfg.Prefix+"factory-"+s.host()+"-")
	if err != nil {
		return nil, err
	}
	var out []Object
	for _, o := range objs {
		if h, _, ok := parseName(baseName(o.Key)); ok && h == s.host() {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		_, a, _ := parseName(baseName(out[i].Key))
		_, b, _ := parseName(baseName(out[j].Key))
		return a.After(b)
	})
	return out, nil
}

// prune deletes this host's archives (and their sidecars) that retention
// does not keep. It runs only after a successful upload.
func (s Service) prune(ctx context.Context, b Bucket, cfg config.Backup) ([]string, error) {
	del, ok := b.(deleter)
	if !ok {
		return nil, nil
	}
	objs, err := s.archives(ctx, b, cfg)
	if err != nil {
		return nil, fmt.Errorf("backup: retention: %w", err)
	}
	names := make([]string, len(objs))
	byName := map[string]string{}
	for i, o := range objs {
		names[i] = baseName(o.Key)
		byName[names[i]] = o.Key
	}
	_, drop := s.retention(cfg).keep(names)
	var deleted []string
	for _, n := range drop {
		k := byName[n]
		if err := del.Delete(ctx, k); err != nil {
			return deleted, fmt.Errorf("backup: retention: %w", err)
		}
		if err := del.Delete(ctx, k+sidecarSuffix); err != nil {
			return deleted, fmt.Errorf("backup: retention: %w", err)
		}
		deleted = append(deleted, k)
	}
	if len(deleted) > 0 {
		s.logf("backup: retention removed %s", strings.Join(deleted, ", "))
	}
	return deleted, nil
}

// Verified is what Verify found in the newest archive.
type Verified struct {
	Key     string
	Files   int
	Tickets int
}

// Verify is `factory backup verify`, every Sunday: the newest archive is
// downloaded, checked against its sidecar, restored into a temporary
// directory, and its config checked and its tickets counted. A failure
// files a notice; a success is silent but for one log line a month (the
// first Sunday's).
func (s Service) Verify(ctx context.Context, cfg config.Backup) (Verified, error) {
	v, err := s.verify(ctx, cfg)
	if err != nil {
		s.logf("backup verify failed: %v", err)
		s.notice(ctx, "Backup check failed", fmt.Sprintf("`factory backup verify` could not restore the newest backup: %v", err))
		return v, err
	}
	if s.Clock.Now().UTC().Day() <= 7 {
		s.logf("backup: the newest backup restores (%s: %d files, %d tickets)", v.Key, v.Files, v.Tickets)
	}
	return v, nil
}

func (s Service) verify(ctx context.Context, cfg config.Backup) (Verified, error) {
	key, err := ReadKey(keyPath(cfg, s.Paths))
	if err != nil {
		return Verified{}, err
	}
	bucket := s.Bucket
	if bucket == nil {
		b, err := Open(cfg, s.Paths)
		if err != nil {
			return Verified{}, err
		}
		bucket = b
	}
	objs, err := s.archives(ctx, bucket, cfg)
	if err != nil {
		return Verified{}, err
	}
	if len(objs) == 0 {
		return Verified{}, fmt.Errorf("backup: no archive of %s under %q", s.host(), cfg.Prefix)
	}
	v := Verified{Key: objs[0].Key}
	work, err := s.workDir("verify-")
	if err != nil {
		return v, err
	}
	defer fsx.RemoveTree(work)
	local := filepath.Join(work, baseName(v.Key))
	if err := download(ctx, bucket, v.Key, local); err != nil {
		return v, err
	}
	into := filepath.Join(work, "restored")
	if v.Files, err = RestoreFile(ctx, local, key, into); err != nil {
		return v, err
	}
	v.Tickets, err = checkRestored(into)
	return v, err
}

// download fetches an archive and checks it against its sidecar.
func download(ctx context.Context, b Bucket, key, local string) error {
	var side strings.Builder
	if _, err := b.Get(ctx, key+sidecarSuffix, &side); err != nil {
		return fmt.Errorf("backup: %s: %w", key+sidecarSuffix, err)
	}
	sum, size, ok := parseSidecar([]byte(side.String()))
	if !ok {
		return fmt.Errorf("backup: %s is not a size and sha256", key+sidecarSuffix)
	}
	f, err := os.OpenFile(local, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	h := sha256.New()
	n, err := b.Get(ctx, key, io.MultiWriter(f, h))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum || n != size {
		return fmt.Errorf("backup: %s is %d bytes, sha256 %s; its sidecar says %d bytes, sha256 %s", key, n, got, size, sum)
	}
	return nil
}

// checkRestored is `factory check` on a restored root: its config parses
// with no problems, and its tickets are counted (every one must read).
func checkRestored(root string) (int, error) {
	cfgBytes, err := os.ReadFile(filepath.Join(root, "config.toml"))
	if err != nil {
		return 0, fmt.Errorf("backup verify: %w", err)
	}
	cfg, err := config.Parse(cfgBytes)
	if err != nil {
		return 0, fmt.Errorf("backup verify: config.toml: %w", err)
	}
	if problems, _ := cfg.Problems(nil); len(problems) > 0 {
		return 0, fmt.Errorf("backup verify: config.toml: %s", strings.Join(problems, "; "))
	}
	p := paths.FromEnv(map[string]string{"FACTORY_ROOT": root, "HOME": root})
	ts, probs := (&boardread.Reader{Paths: p}).List()
	if len(probs) > 0 {
		return len(ts), fmt.Errorf("backup verify: %d ticket files do not read: %v", len(probs), probs[0])
	}
	return len(ts), nil
}

// Restore is `factory backup restore ARCHIVE --to DIR`: ARCHIVE is a local
// file, or else an object key (or name) in the bucket. DIR must be empty
// or not exist, and must not be the live root or inside it.
func (s Service) Restore(ctx context.Context, cfg config.Backup, archive, dir string) (int, error) {
	if err := s.refuseTarget(dir); err != nil {
		return 0, err
	}
	key, err := ReadKey(keyPath(cfg, s.Paths))
	if err != nil {
		return 0, err
	}
	if fi, err := os.Stat(archive); err == nil && fi.Mode().IsRegular() {
		return RestoreFile(ctx, archive, key, dir)
	}
	bucket := s.Bucket
	if bucket == nil {
		b, err := Open(cfg, s.Paths)
		if err != nil {
			return 0, err
		}
		bucket = b
	}
	objKey := archive
	if !strings.Contains(archive, "/") && cfg.Prefix != "" && !strings.HasPrefix(archive, cfg.Prefix) {
		objKey = cfg.Prefix + archive
	}
	work, err := s.workDir("restore-")
	if err != nil {
		return 0, err
	}
	defer fsx.RemoveTree(work)
	local := filepath.Join(work, baseName(objKey))
	if err := download(ctx, bucket, objKey, local); err != nil {
		return 0, err
	}
	return RestoreFile(ctx, local, key, dir)
}

func (s Service) refuseTarget(dir string) error {
	if s.Paths.Root == "" {
		return nil
	}
	in, err := within(s.Paths.Root, dir)
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	if in {
		return fmt.Errorf("restore: %s is the live root or inside it; restore into an empty directory elsewhere", dir)
	}
	return nil
}

// RestoreFile decrypts and unpacks a local archive into dir, which must be
// empty or not exist. It returns how many files it wrote.
func RestoreFile(ctx context.Context, archive string, key []byte, dir string) (int, error) {
	if err := emptyDir(dir); err != nil {
		return 0, err
	}
	f, err := os.Open(archive)
	if err != nil {
		return 0, fmt.Errorf("restore: %w", err)
	}
	defer f.Close()
	plain, err := Decrypt(bufio.NewReader(f), key)
	if err != nil {
		return 0, err
	}
	return extract(ctx, plain, dir)
}

// emptyDir makes dir (0700) or checks that it is an empty directory.
func emptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("restore: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("restore: %s is not empty; restore only into an empty directory", dir)
	}
	return nil
}
