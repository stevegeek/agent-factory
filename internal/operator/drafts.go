package operator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/gitread"
	"github.com/stevegeek/agent-factory/internal/gitx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
	"github.com/stevegeek/agent-factory/internal/tiers"
)

// maxBlob is the largest file content read for the checker; a bigger one
// counts as binary (refused).
const maxBlob = 4 << 20

// landing is the config project drafts land in, when the factory lands
// them at all: a project with self_deploy = "config", and
// [deploy] naming the same repository, so that what merges is what the
// deploy takes.
func landing(cfg *config.Config) (*config.Project, string) {
	p, ok := cfg.SelfDeployProject(config.SelfDeployConfig)
	switch {
	case !ok:
		return nil, "no config project (self_deploy = \"config\")"
	case !cfg.Deploy.On():
		return nil, "no [deploy] in config"
	case !strings.EqualFold(p.GitHub, cfg.Deploy.Config):
		return nil, "the config project is not [deploy] config"
	case p.Base == "":
		return nil, "the config project has no base"
	}
	return p, ""
}

// itemName is a draft's fixed name: <ticket lower>-<run>-<12 hex of the
// SHA-256 of its title and patch>. The work dir and the branch carry it,
// so every attempt at one draft uses the same branch (and finds its PR);
// being the draft's content, not its place on the ticket, a rerun that
// drops an earlier draft never hands its branch to another.
func itemName(t *ticket.Ticket, i int) string {
	d := t.Drafts[i]
	sum := sha256.Sum256([]byte(d.Title + "\x00" + d.Patch))
	return fmt.Sprintf("%s-%d-%s", strings.ToLower(string(t.ID)), d.Run, hex.EncodeToString(sum[:6]))
}

// tapNote is said of a requested item with no tap of the maintainer's.
const tapNote = "requested, but not the maintainer's tap (only the maintainer's Apply it on the desk is): judged as new; "

// draft acts on one new or requested draft: by hand when the factory does
// not land drafts; otherwise judged on config main and refused, left to
// wait or landed. line is what the settle move says of it ("" for
// nothing); merged is a draft merged in this pass.
func (s Service) draft(ctx context.Context, cfg *config.Config, t *ticket.Ticket, i int, trusted bool) (line string, merged bool) {
	d := &t.Drafts[i]
	p, off := landing(cfg)
	if p == nil {
		d.State, d.Note = model.ItemByHand, "the factory does not land config drafts here ("+off+"): apply it by hand"
		return "", false
	}
	requested := ticket.DraftState(*d) == model.ItemRequested
	if requested && !ticket.Tapped(t, d.Run, d.Requested, cfg.Maintainer()) {
		requested, d.State, d.Requested = false, model.ItemNew, ""
		defer func() { d.Note = textx.Clip(tapNote+d.Note, 2000) }()
	}
	name := itemName(t, i)
	dir := filepath.Join(s.Paths.OperatorWork(), name)
	defer s.removeWork(dir)
	w, err := s.prepare(ctx, cfg, *p, *d, dir)
	if err != nil {
		s.fail(d, err)
		return d.Title + ": failed", false
	}
	if w.onMain {
		d.State, d.Note = model.ItemApplied, "already on config main"
		return d.Title + ": already on config main", false
	}
	v := w.verdict
	d.Tier, d.Places, d.Walls = string(v.Tier), v.Places, v.Walls
	d.Note = textx.Clip(strings.Join(v.Reasons, "; "), 2000)
	switch {
	case v.Tier == model.TierNever:
		d.State = model.ItemRefused
		d.Note = textx.Clip("only you, by hand: "+strings.Join(v.Never, ", ")+"; "+d.Note, 2000)
		return d.Title + ": refused (" + textx.Clip(strings.Join(v.Never, ", "), 200) + ")", false
	case !requested && v.Tier == model.TierAuto && !trusted:
		d.State = model.ItemWaiting
		d.Note = textx.Clip("the ticket holds words that are not yours, so nothing applies by itself; "+d.Note, 2000)
		return "", false
	case !requested && v.Tier != model.TierAuto:
		d.State = model.ItemWaiting
		return "", false
	}
	if err := s.land(ctx, cfg, *p, t, d, w, "bot/operator-"+name); err != nil {
		s.fail(d, err)
		return d.Title + ": failed", false
	}
	d.State = model.ItemApplied
	d.Note = joinNote(d.Note, "merged as PR #"+strconv.FormatInt(d.PR, 10))
	return fmt.Sprintf("%s: PR #%d merged as %s", d.Title, d.PR, textx.Prefix(d.Commit, 12)), true
}

// fail records an error on a draft: the first runes of it, and the PR an
// earlier step left open.
func (s Service) fail(d *ticket.Draft, err error) {
	d.State, d.Note = model.ItemFailed, textx.Prefix(err.Error(), noteRunes)
	if d.PR != 0 {
		d.Note += "; PR #" + strconv.FormatInt(d.PR, 10) + " open; Apply it again reuses it"
	}
}

func (s Service) removeWork(dir string) {
	if err := fsx.RemoveTree(dir); err != nil {
		s.logf("operator: could not remove %s: %v", dir, err)
	}
}

// work is a draft applied in a temp clone of config main.
type work struct {
	clone   *gitx.Clone
	base    string // the main commit the draft was applied to
	onMain  bool   // the draft's reverse applies: it is on main already
	verdict tiers.Verdict
}

// prepare applies a draft to config main in a temp clone at dir and judges
// the result: the project's clone is refreshed from GitHub, cloned without
// a checkout and detached at its main, so the checker, the commit and the
// push all see one main commit. A draft whose reverse applies is on main
// already. The checker reads the files from git's objects, never through
// the work tree (a symlink there could point anywhere).
func (s Service) prepare(ctx context.Context, cfg *config.Config, p config.Project, d ticket.Draft, dir string) (*work, error) {
	if s.Repos == nil {
		return nil, errors.New("operator: no repositories")
	}
	if s.GitHub == nil {
		return nil, errors.New("no GitHub App to read config main")
	}
	if strings.TrimSpace(d.Patch) == "" {
		return nil, errors.New("the draft has no patch")
	}
	if textx.RuneLen(d.Patch) > maxPatch {
		return nil, fmt.Errorf("the patch is longer than %d characters", maxPatch)
	}
	repo := s.Repos(p)
	wr := gitx.Writer{Repo: repo}
	if err := wr.RefreshBase(ctx); err != nil {
		return nil, fmt.Errorf("refresh config %s: %w", p.Base, err)
	}
	// the base is GitHub's main, never the clone's (agent-writable): a
	// commit planted there would be the checker's base and ride along in
	// the push
	base, err := s.GitHub.BranchHead(ctx, p.GitHub, p.Base)
	if err != nil {
		return nil, fmt.Errorf("config %s on GitHub: %w", p.Base, err)
	}
	local, err := repo.Head(ctx, p.Base)
	if err != nil {
		return nil, err
	}
	if local != base {
		return nil, fmt.Errorf("the config clone's %s (%s) is not GitHub's (%s): nothing lands until it is", p.Base, textx.Prefix(local, 12), textx.Prefix(base, 12))
	}
	if err := fsx.RemoveTree(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, err
	}
	clone, err := wr.CloneNoCheckout(ctx, dir)
	if err != nil {
		return nil, err
	}
	if err := clone.CheckoutDetach(ctx, base); err != nil {
		return nil, err
	}
	w := &work{clone: clone, base: base}
	res, err := s.git(ctx, clone, d.Patch, "apply", "--reverse", "--check")
	if err != nil {
		return nil, err
	}
	if res.Code == 0 {
		w.onMain = true
		return w, nil
	}
	res, err = s.git(ctx, clone, d.Patch, "apply", "--index")
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		return nil, errors.New("the draft does not apply to config main: " + textx.Strip(strings.ToValidUTF8(string(res.Stderr), "?")))
	}
	files, err := s.files(ctx, clone, base)
	if err != nil {
		return nil, err
	}
	w.verdict = tiers.Draft(tiers.In{ConfigDir: cfg.Deploy.ConfigDir, FactoryHome: cfg.Host.FactoryHome, Files: files})
	return w, nil
}

// git runs git in the temp clone with the safe environment, stdin as
// given ("" is none). A non-zero exit is in the result.
func (s Service) git(ctx context.Context, c *gitx.Clone, stdin string, args ...string) (execx.Result, error) {
	cmd := execx.Cmd{Argv: append([]string{"git", "-C", c.Dir}, args...), Env: gitread.SafeEnv(c.Env), Timeout: gitTimeout}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	res, err := c.Exec.Run(ctx, cmd)
	if err != nil {
		return res, fmt.Errorf("git %s: %w", args[0], err)
	}
	return res, nil
}

// gitOK is git that must exit 0.
func (s Service) gitOK(ctx context.Context, c *gitx.Clone, args ...string) (execx.Result, error) {
	res, err := s.git(ctx, c, "", args...)
	if err != nil {
		return res, err
	}
	return res, execx.CheckErr(res)
}

// change is one entry of `git diff-index --raw`.
type change struct {
	oldMode, newMode, oldSHA, newSHA string
	status                           byte
	path, from                       string
}

// files are the files the applied draft changes, read from the index
// against base: modes and renames from --raw, binary from --numstat, the
// contents from the objects.
func (s Service) files(ctx context.Context, c *gitx.Clone, base string) ([]tiers.File, error) {
	raw, err := s.gitOK(ctx, c, "diff-index", "--cached", "--raw", "--no-abbrev", "-z", "-M", "-C", base)
	if err != nil {
		return nil, err
	}
	changes, err := parseRaw(raw.Stdout)
	if err != nil {
		return nil, err
	}
	num, err := s.gitOK(ctx, c, "diff-index", "--cached", "--numstat", "-z", "-M", "-C", base)
	if err != nil {
		return nil, err
	}
	binary := parseNumstat(num.Stdout)
	var out []tiers.File
	for _, ch := range changes {
		f := tiers.File{Path: ch.path, From: ch.from, Binary: binary[ch.path],
			Created: ch.status == 'A', Deleted: ch.status == 'D'}
		if ch.oldMode != "000000" {
			f.OldMode = ch.oldMode
		}
		switch {
		case f.Deleted:
		case f.Created || ch.newMode != ch.oldMode:
			f.Mode = ch.newMode
		}
		if f.Old, err = s.blob(ctx, c, ch.oldSHA); err != nil {
			return nil, err
		}
		if f.New, err = s.blob(ctx, c, ch.newSHA); err != nil {
			return nil, err
		}
		if f.Old == nil && ch.oldSHA != zeroSHA || f.New == nil && ch.newSHA != zeroSHA ||
			bytes.IndexByte(f.Old, 0) >= 0 || bytes.IndexByte(f.New, 0) >= 0 {
			f.Binary = true
		}
		out = append(out, f)
	}
	return out, nil
}

const zeroSHA = "0000000000000000000000000000000000000000"

// blob is an object's content: nil for the zero id (no file) and for one
// larger than maxBlob.
func (s Service) blob(ctx context.Context, c *gitx.Clone, sha string) ([]byte, error) {
	if sha == "" || strings.Trim(sha, "0") == "" {
		return nil, nil
	}
	cmd := execx.Cmd{Argv: []string{"git", "-C", c.Dir, "cat-file", "blob", sha}, Env: gitread.SafeEnv(c.Env), Timeout: gitTimeout, MaxOut: maxBlob}
	res, err := c.Exec.Run(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("git cat-file: %w", err)
	}
	if err := execx.CheckErr(res); err != nil {
		if res.Code != 0 && strings.Contains(string(res.Stderr), "bad file") {
			return nil, nil // a gitlink's commit is no blob
		}
		return nil, err
	}
	if res.Truncated {
		return nil, nil
	}
	if res.Stdout == nil {
		return []byte{}, nil
	}
	return res.Stdout, nil
}

// parseRaw reads `diff-index --raw -z`: ":<old mode> <new mode> <old sha>
// <new sha> <status>" NUL <path> NUL, with a second path for a rename or
// copy (its source first).
func parseRaw(out []byte) ([]change, error) {
	tok := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	var cs []change
	for i := 0; i < len(tok); i++ {
		if tok[i] == "" {
			continue
		}
		h := strings.Fields(strings.TrimPrefix(tok[i], ":"))
		if !strings.HasPrefix(tok[i], ":") || len(h) != 5 || h[4] == "" {
			return nil, fmt.Errorf("git diff-index: cannot read %q", tok[i])
		}
		c := change{oldMode: h[0], newMode: h[1], oldSHA: h[2], newSHA: h[3], status: h[4][0]}
		n := 1
		if c.status == 'R' || c.status == 'C' {
			n = 2
		}
		if i+n >= len(tok) {
			return nil, fmt.Errorf("git diff-index: short entry %q", tok[i])
		}
		if n == 2 {
			c.from, c.path = tok[i+1], tok[i+2]
		} else {
			c.path = tok[i+1]
		}
		i += n
		cs = append(cs, c)
	}
	return cs, nil
}

// parseNumstat reads `diff-index --numstat -z` into the paths git counts
// as binary ("-" lines). A rename's entry names its destination.
func parseNumstat(out []byte) map[string]bool {
	tok := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	bin := map[string]bool{}
	for i := 0; i < len(tok); i++ {
		parts := strings.SplitN(tok[i], "\t", 3)
		if len(parts) != 3 {
			continue
		}
		path := parts[2]
		if path == "" && i+2 < len(tok) {
			path = tok[i+2]
			i += 2
		}
		if parts[0] == "-" || parts[1] == "-" {
			bin[path] = true
		}
	}
	return bin
}

// land commits the applied draft as the factory's identity, puts it in the
// project's clone as branch, pushes it, opens its PR (or finds the one an
// earlier attempt opened), waits for GitHub to work out mergeability, and
// merges the PR at the pushed commit: GitHub refuses if the head moved.
// The PR and merge commit are recorded on the draft as they become known.
func (s Service) land(ctx context.Context, cfg *config.Config, p config.Project, t *ticket.Ticket, d *ticket.Draft, w *work, branch string) error {
	if s.GitHub == nil || s.Token == nil {
		return errors.New("no GitHub App for the push")
	}
	sha, err := s.commit(ctx, cfg, t, d, w)
	if err != nil {
		return err
	}
	wr := gitx.Writer{Repo: s.Repos(p)}
	if err := s.toProject(ctx, wr, w, branch, sha); err != nil {
		return err
	}
	defer func() {
		if err := wr.DeleteBranch(context.WithoutCancel(ctx), branch); err != nil {
			s.logf("operator: could not delete the local %s: %v", branch, err)
		}
	}()
	tok, err := s.Token(ctx)
	if err != nil {
		return fmt.Errorf("github: installation token: %w", err)
	}
	if err := wr.Push(ctx, branch, tok); err != nil {
		return err
	}
	pr, err := s.openPR(ctx, cfg, p, t, d, branch)
	if err != nil {
		return err
	}
	d.PR, d.PRURL = pr.Number, pr.HTMLURL
	if base, _ := pr.Raw.Get("base").Get("ref").Str(); base != p.Base {
		return fmt.Errorf("PR #%d is against %q, not %s; it is not merged", pr.Number, base, p.Base)
	}
	for n := 0; pr.MergeableState == "unknown" && n < mergePolls; n++ {
		if err := s.sleep(ctx, mergePoll); err != nil {
			return err
		}
		if pr, err = s.GitHub.PR(ctx, p.GitHub, pr.Number); err != nil {
			return err
		}
	}
	if err := s.mergeable(ctx, p, w, d.PR, sha); err != nil {
		return err
	}
	res, err := s.GitHub.Merge(ctx, p.GitHub, d.PR, sha)
	if err != nil {
		return err
	}
	if !res.Merged {
		return fmt.Errorf("GitHub did not merge PR #%d: %s", d.PR, res.Message)
	}
	d.Commit = res.SHA
	if err := s.GitHub.DeleteRef(ctx, p.GitHub, branch); err != nil {
		s.logf("operator: merged PR #%d, but could not delete %s: %v", d.PR, branch, err)
	}
	return nil
}

// mergeable checks, just before the merge, that the PR is exactly what was
// judged: its head the pushed commit, one commit on base, and GitHub's
// main still the commit the draft was judged on (a main that moved would
// merge something the checker never saw). GitHub's merge pins only the
// head, so a push to main in the moment between this read and the merge
// is not caught; config main takes only reviewed merges.
func (s Service) mergeable(ctx context.Context, p config.Project, w *work, number int64, sha string) error {
	pr, err := s.GitHub.PR(ctx, p.GitHub, number)
	if err != nil {
		return err
	}
	commits, _ := pr.Raw.Get("commits").Int64()
	base, _ := pr.Raw.Get("base").Get("ref").Str()
	switch {
	case pr.HeadSHA != sha:
		return fmt.Errorf("PR #%d's head is %s, not the pushed %s; it is not merged", number, textx.Prefix(pr.HeadSHA, 12), textx.Prefix(sha, 12))
	case commits != 1:
		return fmt.Errorf("PR #%d has %d commits, not the draft's one; it is not merged", number, commits)
	case base != p.Base:
		return fmt.Errorf("PR #%d is against %q, not %s; it is not merged", number, base, p.Base)
	}
	head, err := s.GitHub.BranchHead(ctx, p.GitHub, p.Base)
	if err != nil {
		return err
	}
	if head != w.base {
		return fmt.Errorf("config %s moved since the draft was judged (%s, now %s); it is not merged: Apply it again judges it on the new %s",
			p.Base, textx.Prefix(w.base, 12), textx.Prefix(head, 12), p.Base)
	}
	return nil
}

// commit commits the index in the temp clone.
func (s Service) commit(ctx context.Context, cfg *config.Config, t *ticket.Ticket, d *ticket.Draft, w *work) (string, error) {
	name, email := cfg.Identity.GitName, cfg.Identity.GitEmail
	if name == "" {
		name = "factory"
	}
	if email == "" {
		email = "factory@localhost"
	}
	// the subject is one line of at most 72 runes; a longer title is
	// whole in the body
	msg := textx.Clip(d.Title, 72) + "\n\n"
	if full := textx.OneLine(d.Title); full != textx.Clip(d.Title, 72) {
		msg += full + "\n\n"
	}
	if why := strings.TrimSpace(d.Why); why != "" {
		msg += why + "\n\n"
	}
	msg += "Operator draft from " + string(t.ID) + ", " + model.TierLabel(model.Tier(d.Tier)) + "."
	if _, err := s.gitOK(ctx, w.clone, "-c", "user.name="+name, "-c", "user.email="+email, "-c", "commit.gpgsign=false",
		"commit", "-q", "--no-verify", "-m", msg); err != nil {
		return "", err
	}
	res, err := s.gitOK(ctx, w.clone, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return textx.Strip(string(res.Stdout)), nil
}

// toProject puts the commit in the project's clone as branch, through a
// bundle the clone fetches with every object checked.
func (s Service) toProject(ctx context.Context, wr gitx.Writer, w *work, branch, sha string) error {
	if _, err := s.gitOK(ctx, w.clone, "update-ref", "refs/heads/"+branch, sha); err != nil {
		return err
	}
	path := filepath.Join(w.clone.Dir, ".git", "operator.bundle")
	if _, err := s.gitOK(ctx, w.clone, "bundle", "create", "-q", path, "refs/heads/"+branch, "^"+w.base); err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return wr.FetchBundle(ctx, branch, b)
}

// openPR finds the open PR of branch, or opens one.
func (s Service) openPR(ctx context.Context, cfg *config.Config, p config.Project, t *ticket.Ticket, d *ticket.Draft, branch string) (*github.PR, error) {
	owner, _, _ := strings.Cut(p.GitHub, "/")
	title := textx.Clip(d.Title, 72)
	pr, err := s.GitHub.FindOpenPR(ctx, p.GitHub, owner, branch)
	if err != nil || pr != nil {
		return ours(pr, title, branch, err)
	}
	pr, err = s.GitHub.CreatePR(ctx, p.GitHub, github.NewPR{Title: title, Body: prBody(cfg, t, d), Head: branch, Base: p.Base})
	var he *github.HTTPError
	if errors.As(err, &he) && he.Code == 422 {
		if again, ferr := s.GitHub.FindOpenPR(ctx, p.GitHub, owner, branch); ferr == nil && again != nil {
			return ours(again, title, branch, nil)
		}
	}
	return pr, err
}

// ours passes on a PR found on the draft's branch only when its title is
// the draft's: a PR another draft opened (its title, body, tier and places
// say what that one was) is not reused.
func ours(pr *github.PR, title, branch string, err error) (*github.PR, error) {
	if err != nil || pr == nil {
		return pr, err
	}
	if got, _ := pr.Raw.Get("title").Str(); got != title {
		return nil, fmt.Errorf("the open PR #%d on %s is another draft's (%q); close it and Apply it again", pr.Number, branch, textx.Clip(got, 80))
	}
	return pr, nil
}

// prBody is the draft's why, its tier and places, and the PR footer.
func prBody(cfg *config.Config, t *ticket.Ticket, d *ticket.Draft) string {
	var b strings.Builder
	if why := strings.TrimSpace(d.Why); why != "" {
		b.WriteString(why + "\n\n")
	}
	b.WriteString("Operator draft from " + string(t.ID) + ": " + model.TierLabel(model.Tier(d.Tier)) + " (" + d.Tier + ").\n")
	if len(d.Places) > 0 {
		b.WriteString("\nPlaces: " + strings.Join(d.Places, ", ") + "\n")
	}
	if len(d.Walls) > 0 {
		b.WriteString("Walls: " + strings.Join(d.Walls, ", ") + "\n")
	}
	if f := strings.TrimSpace(cfg.Identity.PRFooter); f != "" {
		b.WriteString("\n" + f + "\n")
	}
	return b.String()
}
