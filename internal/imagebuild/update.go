package imagebuild

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
)

// Automatic agent updates. The weekly rebuild updates Claude Code and Pi
// by itself: the [images] pins are a floor, the version installed lives in
// ~/factory/images.json (which a deploy never writes), and the effective
// version is the newer of the two. A newer release is built as the
// candidate, smoke-checked for that exact version, checked against the
// projects (blocking only a regression), and only then promoted. `factory
// rebuild --rollback KIND` goes back to :previous and holds the version it
// left; a held version is not tried again until a newer one is out or
// `--unhold KIND`. images.json's previous always names the version
// :previous holds: every promoted rebuild, update or not, retags the image
// it replaces as :previous (Rebuilt).

// ToolState is one tool's entry in images.json.
type ToolState struct {
	Version  string `json:"version"`
	Since    string `json:"since,omitempty"`
	Previous string `json:"previous,omitempty"`
	// Held is a version rolled back from: not tried again until a newer
	// one is out (or --unhold).
	Held string `json:"held,omitempty"`
}

// State is images.json: by kind ("claude", "pi").
type State map[string]ToolState

// LoadState reads images.json; a missing file is an empty State.
func LoadState(path string) (State, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("images.json: %w", err)
	}
	st := State{}
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("%s: %w", path, err)
	}
	return st, nil
}

// SaveState writes images.json atomically (keys sorted, two-space indent).
func SaveState(path string, st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("images.json: %w", err)
	}
	if err := fsx.WriteAtomic(path, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("images.json: %w", err)
	}
	return nil
}

// Effective is the version an image has: the newer of the config floor
// and images.json ("" when neither names one).
func Effective(floor string, st ToolState) string {
	switch {
	case st.Version == "":
		return floor
	case floor == "":
		return st.Version
	case Newer(floor, st.Version):
		return floor
	}
	return st.Version
}

// Prerelease reports a version with a pre-release part (1.2.3-beta.1).
func Prerelease(v string) bool {
	return strings.ContainsAny(v, "-+") || v == ""
}

// Decision is what the rebuild does for one image.
type Decision struct {
	Target string // the version to build; "" builds the Containerfile's default
	From   string // the effective version before
	Update bool   // Target is a newer release than From: promote and record it
	Why    string // why a release was not taken (a log line), or ""
}

// Decide applies the rule: with auto-update on and a stable latest newer
// than the effective version and than any held one, build the latest;
// otherwise rebuild the effective version.
func Decide(floor string, st ToolState, auto bool, latest string) Decision {
	d := Decision{From: Effective(floor, st)}
	d.Target = d.From
	switch {
	case !auto || latest == "":
	case Prerelease(latest):
		d.Why = latest + " is a pre-release"
	case d.From != "" && !Newer(latest, d.From):
	case st.Held != "" && !Newer(latest, st.Held):
		d.Why = fmt.Sprintf("%s is held (rolled back); `factory rebuild --unhold` to try it again", st.Held)
	default:
		d.Target, d.Update = latest, true
	}
	return d
}

// AutoUpdated reports whether kind is in the auto_update list.
func AutoUpdated(list []string, kind string) bool { return slices.Contains(list, kind) }

// Promoted records a promoted update in st.
func Promoted(st State, kind string, d Decision, now time.Time) {
	st[kind] = ToolState{Version: d.Target, Since: clock.Stamp(now), Previous: d.From}
}

// Rebuilt records, in st, that a rebuild with no update was promoted: every
// promoted rebuild retags the image it replaces as :previous, so :previous
// now holds version, the one the live image already had. Previous always
// names what :previous holds, so a rollback records the truth. A kind with
// no entry (its image at the floor, never updated) gets none: its
// :previous has the floor too. It reports whether st changed.
func Rebuilt(st State, kind, version string) bool {
	cur, ok := st[kind]
	if !ok || cur.Previous == version {
		return false
	}
	cur.Previous = version
	st[kind] = cur
	return true
}

// Rollback retags <repo>:previous as tag and records, in st, the version
// it goes back to (the one :previous holds), holding the one it leaves.
// When :previous has the live image's own version (a rebuild with no update
// since the last update, or a kind never updated), the retag still undoes
// the last rebuild's fresh packages, but the version does not change and
// nothing is held: the returned state then has Held "".
func Rollback(ctx context.Context, p Tagger, st State, kind, tag string, now time.Time) (ToolState, error) {
	if tag == "" {
		return ToolState{}, fmt.Errorf("rollback %s: no image is configured for it", kind)
	}
	r, err := p.Tag(ctx, Repo(tag)+":previous", tag)
	if err == nil {
		err = execx.CheckErr(r)
	}
	if err != nil {
		return ToolState{}, fmt.Errorf("rollback %s: %w", kind, err)
	}
	cur, ok := st[kind]
	if !ok || cur.Previous == "" || cur.Previous == cur.Version {
		return ToolState{Version: cur.Version, Since: cur.Since, Previous: cur.Previous, Held: cur.Held}, nil
	}
	next := ToolState{Version: cur.Previous, Since: clock.Stamp(now), Previous: cur.Previous, Held: cur.Version}
	st[kind] = next
	return next, nil
}

// Unhold clears a held version.
func Unhold(st State, kind string) bool {
	cur, ok := st[kind]
	if !ok || cur.Held == "" {
		return false
	}
	cur.Held = ""
	st[kind] = cur
	return true
}

// Tagger tags images (podman.Client does).
type Tagger interface {
	Tag(ctx context.Context, src, dst string) (execx.Result, error)
}

// DefaultNPM is the npm registry.
const DefaultNPM = "https://registry.npmjs.org"

// NPMTimeout bounds one registry lookup.
const NPMTimeout = 30 * time.Second

// NPMLatest is the "latest" dist-tag of an npm package (the helper's
// lookup; the dispatcher's goes through github.Client.NPMLatest, which
// reads the same document).
func NPMLatest(ctx context.Context, client *http.Client, base, pkg string) (string, error) {
	if base == "" {
		base = DefaultNPM
	}
	if client == nil {
		client = &http.Client{}
	}
	ctx, cancel := context.WithTimeout(ctx, NPMTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/"+pkg+"/latest", nil)
	if err != nil {
		return "", fmt.Errorf("npm %s: %w", pkg, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("npm %s: %w", pkg, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("npm %s: %w", pkg, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("npm %s: HTTP %d", pkg, resp.StatusCode)
	}
	var doc struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&doc); err != nil || doc.Version == "" {
		return "", fmt.Errorf("npm %s: no version in the answer", pkg)
	}
	return doc.Version, nil
}

// hasVersion reports whether the version command's output names exactly
// v: as a whole token, so 2.1.30 is not found in 2.1.302.
func hasVersion(out, v string) bool {
	if v == "" {
		return true
	}
	for _, f := range strings.FieldsFunc(out, func(r rune) bool {
		return !(r == '.' || r == '-' || r == '+' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	}) {
		if f == v || f == "v"+v {
			return true
		}
	}
	return false
}
