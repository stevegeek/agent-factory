package gate

import (
	"embed"
	"errors"
	"fmt"
	"strings"
	"text/template"
	"unicode/utf16"

	"github.com/stevegeek/agent-factory/internal/config"
)

// The off-box gate: the same mechanical checks as Gate 1, run by GitHub on
// every PR as one stable check ("factory gate") a ruleset can require. It
// approves a PR only when the factory opened it (its App, a bot/ branch),
// the factory's own gate called it eligible, and GitHub's run agrees on
// tests, lint, size and paths. A compromised box cannot approve its own PR:
// the approval comes from GitHub's run.

//go:embed workflow.yml.tmpl
var workflowFS embed.FS

// DefaultAmeba is the ameba release the Crystal setup builds when [images]
// pins none.
const DefaultAmeba = "1.7.0"

// workflowSetup is GATE_SETUP: the language's setup steps.
func workflowSetup(crystal bool, ameba string) string {
	if crystal {
		return "      - uses: crystal-lang/install-crystal@v1\n" +
			"      - name: ameba\n" +
			"        run: |\n" +
			"          git clone --depth 1 --branch v" + ameba + " https://github.com/crystal-ameba/ameba /tmp/ameba\n" +
			"          (cd /tmp/ameba && make && sudo install -m 0755 bin/ameba /usr/local/bin/ameba)"
	}
	return "      - uses: ruby/setup-ruby@v1\n" +
		"        with:\n" +
		"          bundler-cache: true"
}

// Workflow is gate_workflow (factory gate-workflow PROJECT): the GitHub
// Actions workflow for a factory project. Its gate job (read-only, no
// persisted credentials) runs the setup, the project's tests and lint, and
// the size-and-paths rule; its approve job (pull-requests: write, no
// checkout) approves only an eligible, non-draft PR the factory's App opened
// from a bot/ branch with "eligible for automatic approval." in its body.
//
// An unknown or board project is refused, with cmd_gate_workflow's words.
// [github] app_slug is required: the code holds no default App (D15).
func Workflow(cfg *config.Config, key string) (string, error) {
	var p *config.Project
	for _, fp := range cfg.FactoryProjects() {
		if fp.Key == key {
			p = &fp
			break
		}
	}
	if p == nil {
		if _, ok := cfg.Project(key); ok {
			return "", fmt.Errorf("unknown project %s (a board project has no gate)", key)
		}
		return "", fmt.Errorf("unknown project %s", key)
	}
	if cfg.GitHub.AppSlug == "" {
		return "", errors.New("[github] app_slug is not set: the gate workflow approves only PRs that app opened")
	}
	crystal := cfg.Toolchain(*p) == "crystal"
	install := "bundle install"
	if crystal {
		install = "shards install"
	}
	ameba, ok := cfg.Images["ameba"]
	if !ok {
		ameba = DefaultAmeba
	}
	base := "main"
	if p.Has("base") {
		base = p.Base
	}
	q := func(cmd string) string { // json.dumps(cmd or "true"): a YAML double-quoted scalar
		if cmd == "" {
			cmd = "true"
		}
		return pyJSONString(cmd)
	}
	tmpl, err := template.New("workflow.yml.tmpl").Delims("[%", "%]").Option("missingkey=error").ParseFS(workflowFS, "workflow.yml.tmpl")
	if err != nil {
		return "", fmt.Errorf("gate workflow: %w", err)
	}
	var b strings.Builder
	err = tmpl.Execute(&b, map[string]any{
		"Project":  key,
		"Base":     base,
		"Setup":    workflowSetup(crystal, ameba),
		"Install":  q(install),
		"Test":     q(p.Test),
		"Lint":     q(p.Lint),
		"Allowed":  pyJSONList(cfg.Gate.AllowedPaths),
		"MaxLines": cfg.MaxChangedLines(),
		"Bot":      cfg.GitHub.AppSlug + "[bot]",
	})
	if err != nil {
		return "", fmt.Errorf("gate workflow: %w", err)
	}
	return b.String(), nil
}

// pyJSONString is Python's json.dumps(s): ensure_ascii, so everything
// outside printable ASCII is a \u escape (astral characters as surrogate
// pairs), with the short escapes for \b \f \n \r \t.
func pyJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r >= 0x20 && r <= 0x7e:
			b.WriteRune(r)
		case r > 0xffff:
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(&b, `\u%04x\u%04x`, hi, lo)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// pyJSONList is json.dumps of a list of strings: ["a", "b"].
func pyJSONList(xs []string) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = pyJSONString(x)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
