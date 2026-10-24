// Command factory is the dispatcher: the CLI, the tick, the doors, upkeep and
// render. Everything is in internal/cli.
package main

import (
	"os"
	"strings"

	"github.com/stevegeek/agent-factory/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args, cli.Env{
		Stdin:         os.Stdin,
		Stdout:        os.Stdout,
		Stderr:        os.Stderr,
		Environ:       environ(os.Environ()),
		HandleSignals: true,
	}))
}

// environ is the process environment as a map; the first of a repeated
// name wins, as getenv finds it.
func environ(kv []string) map[string]string {
	m := make(map[string]string, len(kv))
	for _, s := range kv {
		k, v, ok := strings.Cut(s, "=")
		if _, seen := m[k]; ok && !seen {
			m[k] = v
		}
	}
	return m
}
