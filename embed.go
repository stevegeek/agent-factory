// Package agentfactory is the module root. It holds the data files that ship
// inside the binaries: the role templates, the systemd units, the image
// contexts, the skills and the example config. `factory render` and
// `factory patch-scion` read them from here, so they work from the binary
// alone.
package agentfactory

import (
	"embed"
	"io/fs"
)

// The all: prefix keeps dotfiles (for example an image's .dockerignore).
//
//go:embed all:templates all:systemd all:images all:skills all:examples
var assets embed.FS

// Assets is the embedded data tree, rooted at the module root: paths look
// like "templates/ruby-gem/worker/system-prompt.md".
var Assets fs.FS = assets
