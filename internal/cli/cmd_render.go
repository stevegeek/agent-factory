package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/stevegeek/agent-factory/internal/render"
)

// The render command group (§7.3, slice S19): `render CONFIG_DIR OUT_DIR`
// replaces bin/render for deploy, and `patch-scion PROVISION_PY` replaces
// scion/patch_provision.py. Each runner takes its arguments after the
// command name and returns the exit code: 0, 1 for a refusal (the message
// alone on stderr, as sys.exit printed it) or another error ("factory: "
// and the error), 2 for a usage error. The command table (commands.go)
// registers them as Raw runners.

const (
	renderUsage     = "usage: factory render CONFIG_DIR OUT_DIR"
	patchScionUsage = "usage: factory patch-scion PROVISION_PY"
)

// runRender is `factory render CONFIG_DIR OUT_DIR`: it prints the out dir.
func runRender(args []string, stdout, stderr io.Writer) int {
	pos, code, done := renderArgs(args, 2, renderUsage, stdout, stderr)
	if done {
		return code
	}
	if err := render.Render(render.Assets(), pos[0], pos[1]); err != nil {
		return renderFail(stderr, err)
	}
	fmt.Fprintln(stdout, pos[1])
	return 0
}

// runPatchScion is `factory patch-scion PROVISION_PY`: it prints "patched"
// or "already patched".
func runPatchScion(args []string, stdout, stderr io.Writer) int {
	pos, code, done := renderArgs(args, 1, patchScionUsage, stdout, stderr)
	if done {
		return code
	}
	msg, err := render.PatchFile(pos[0])
	if err != nil {
		return renderFail(stderr, err)
	}
	fmt.Fprintln(stdout, msg)
	return 0
}

// renderArgs takes exactly n positionals: -h/--help prints the usage and
// exits 0, `--` ends options, any other option or a wrong count is a usage
// error (2). done says the caller returns code.
func renderArgs(args []string, n int, usage string, stdout, stderr io.Writer) (pos []string, code int, done bool) {
	opts := true
	for _, a := range args {
		switch {
		case opts && a == "--":
			opts = false
		case opts && (a == "-h" || a == "--help"):
			fmt.Fprintln(stdout, usage)
			return nil, 0, true
		case opts && strings.HasPrefix(a, "-") && a != "-":
			fmt.Fprintf(stderr, "%s\nfactory: unrecognized arguments: %s\n", usage, a)
			return nil, 2, true
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) != n {
		fmt.Fprintf(stderr, "%s\nfactory: expected %d argument(s), got %d\n", usage, n, len(pos))
		return nil, 2, true
	}
	return pos, 0, false
}

// renderFail prints err as §7.2 says and returns 1.
func renderFail(stderr io.Writer, err error) int {
	var p render.Problem
	if errors.As(err, &p) {
		fmt.Fprintln(stderr, err.Error())
	} else {
		fmt.Fprintf(stderr, "factory: %v\n", err)
	}
	return 1
}
