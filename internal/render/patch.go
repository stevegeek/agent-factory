package render

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stevegeek/agent-factory/internal/fsx"
)

// The port of scion/patch_provision.py: Scion's local mode passes the HOST
// worktree path as agent_workspace, but the worktree is mounted at
// /repo-root/.scion/agents/<name>/workspace inside the container, so Claude
// showed its folder-trust prompt. The patch makes the harness trust the
// container path too. The anchor, the addition and the marker are
// patch_provision.py's, byte for byte.
const (
	patchAnchor   = "    new_projects: dict[str, Any] = {ctx.workspace: project_settings}\n"
	patchAddition = `
    # agent-factory local patch: in local mode the manifest carries the HOST
    # worktree path, but the worktree is mounted at /repo-root/... inside the
    # container, so trust that path too (upstream fix: pass the container path).
    marker = "/.scion/agents/"
    if marker in ctx.workspace and not ctx.workspace.startswith("/repo-root/"):
        container_ws = "/repo-root" + marker + ctx.workspace.split(marker, 1)[1]
        new_projects[container_ws] = project_settings
`
	patchMarker   = "local patch: in local mode the manifest"
	patchNoAnchor = Problem("anchor not found; provision.py changed upstream, patch by hand")
)

// PatchState is what Patch found.
type PatchState int

const (
	Patched        PatchState = iota + 1 // the addition was inserted
	AlreadyPatched                       // the marker was there; nothing changed
)

func (s PatchState) String() string {
	switch s {
	case Patched:
		return "patched"
	case AlreadyPatched:
		return "already patched"
	}
	return fmt.Sprintf("PatchState(%d)", int(s))
}

// Patch applies the patch to the text of provision.py. The marker anywhere
// means already patched (checked first, as Python does); otherwise the
// anchor line must occur exactly once, and the addition goes right after it.
func Patch(src string) (string, PatchState, error) {
	if strings.Contains(src, patchMarker) {
		return src, AlreadyPatched, nil
	}
	if strings.Count(src, patchAnchor) != 1 {
		return src, 0, patchNoAnchor
	}
	return strings.Replace(src, patchAnchor, patchAnchor+patchAddition, 1), Patched, nil
}

// PatchFile is `patch-scion PROVISION_PY`: it patches the file and returns
// "patched" or "already patched". The file is read as Python reads text
// (UTF-8, universal newlines), and written back only when patched: through
// a link to the file it names, atomically, keeping its mode.
func PatchFile(name string) (string, error) {
	target, err := filepath.EvalSymlinks(name)
	if err != nil {
		return "", fmt.Errorf("patch-scion: %w", err)
	}
	st, err := os.Stat(target)
	if err != nil {
		return "", fmt.Errorf("patch-scion: %w", err)
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("patch-scion: %s is not a regular file", name)
	}
	b, err := os.ReadFile(target)
	if err != nil {
		return "", fmt.Errorf("patch-scion: %w", err)
	}
	text, err := pyText(b)
	if err != nil {
		return "", fmt.Errorf("patch-scion: %s: %w", name, err)
	}
	out, state, err := Patch(text)
	if err != nil {
		return "", err
	}
	if state == Patched {
		if err := fsx.WriteAtomic(target, []byte(out), st.Mode().Perm()); err != nil {
			return "", fmt.Errorf("patch-scion: %w", err)
		}
		if err := os.Chmod(target, st.Mode().Perm()); err != nil {
			return "", fmt.Errorf("patch-scion: %w", err)
		}
	}
	return state.String(), nil
}
