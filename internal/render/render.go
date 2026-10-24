// Package render is the port of bin/render: it renders the files one host
// needs from the embedded core data and a config directory, and it carries
// scion/patch_provision.py as Patch.
//
// It does not ship programs: deploy installs the binaries and bin/pi-run
// from its own file list.
package render

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	agentfactory "github.com/stevegeek/agent-factory"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// Problem is a refusal: what bin/render passed to sys.exit. The CLI prints
// it alone on stderr and exits 1.
type Problem string

func (p Problem) Error() string { return string(p) }

func problemf(format string, args ...any) Problem { return Problem(fmt.Sprintf(format, args...)) }

// Value is one placeholder and the text it becomes.
type Value struct{ Name, Text string }

func (v Value) String() string { return v.Name + "=" + v.Text }

// ValueList is the placeholder values in bin/render's order.
type ValueList []Value

// Get returns the value of a placeholder.
func (vs ValueList) Get(name string) (string, bool) {
	for _, v := range vs {
		if v.Name == name {
			return v.Text, true
		}
	}
	return "", false
}

// patterns are bin/render's regexes, compiled per use (no package state).
type patterns struct {
	placeholder, project, host, login *regexp.Regexp
}

func newPatterns() patterns {
	return patterns{
		// py: \{\{([a-z_]+)\}\}
		placeholder: regexp.MustCompile(`\{\{([a-z_]+)\}\}`),
		// py: [A-Za-z0-9_.-]+
		project: regexp.MustCompile(`\A[A-Za-z0-9_.-]+\z`),
		// py: [A-Za-z0-9.-]+
		host: regexp.MustCompile(`\A[A-Za-z0-9.-]+\z`),
		// py: [^\s\"'\\%$]+
		login: regexp.MustCompile(`\A[^` + textx.PySpaceSet + `"'\\%$]+\z`),
	}
}

// Values is values(cfg): the placeholder values, or the Problem render
// exits with when one is empty or a Foreman flag would not be one word.
func Values(cfg *config.Config) (ValueList, error) {
	return values(cfg, newPatterns())
}

func values(cfg *config.Config, p patterns) (ValueList, error) {
	vs := ValueList{
		{"git_name", cfg.Identity.GitName},
		{"git_email", cfg.Identity.GitEmail},
		{"pr_footer", cfg.Identity.PRFooter},
		{"factory_user", cfg.Host.FactoryUser},
		{"factory_uid", cfg.Host.FactoryUID},
		{"factory_home", cfg.Host.FactoryHome},
		{"rebuild_at", cfg.Upkeep.RebuildAt},
		{"foreman_listen", cfg.Foreman.Listen},
		{"factory_root", strings.TrimRight(cfg.Host.FactoryHome, "/") + "/factory"},
	}
	var missing []string
	for _, v := range vs {
		if v.Text == "" {
			missing = append(missing, v.Name)
		}
	}
	if len(missing) > 0 {
		return nil, problemf("config.toml is missing: %s (see [identity] and [host])", strings.Join(missing, ", "))
	}
	flags, err := foremanFlags(cfg, p)
	if err != nil {
		return nil, err
	}
	return append(vs, Value{"foreman_flags", flags}), nil
}

// ForemanFlags is foreman_flags: the extra arguments of Foreman's unit
// ExecStart. --project for each factory-mode project only (Foreman files
// pipeline tickets; a board project's come through `factory ticket`),
// --host per [foreman] hosts, --trust-tailscale and --tailscale-user. Each
// value must be one word, since systemd splits the line on spaces.
func ForemanFlags(cfg *config.Config) (string, error) {
	return foremanFlags(cfg, newPatterns())
}

func foremanFlags(cfg *config.Config, p patterns) (string, error) {
	var flags []string
	for _, proj := range cfg.FactoryProjects() {
		if !p.project.MatchString(proj.Key) {
			return "", problemf("[projects] not a plain name: %s", pyRepr(proj.Key))
		}
		flags = append(flags, "--project", proj.Key)
	}
	fm := cfg.Foreman
	for _, h := range fm.Hosts {
		if !p.host.MatchString(h) {
			return "", problemf("[foreman] hosts: not a host name: %s", pyRepr(h))
		}
		flags = append(flags, "--host", h)
	}
	if fm.TrustTailscale {
		if len(fm.TailscaleUsers) == 0 {
			return "", Problem("[foreman] trust_tailscale needs tailscale_users")
		}
		flags = append(flags, "--trust-tailscale")
	}
	for _, u := range fm.TailscaleUsers {
		if !p.login.MatchString(u) {
			return "", problemf("[foreman] tailscale_users: not a login: %s", pyRepr(u))
		}
		flags = append(flags, "--tailscale-user", u)
	}
	return strings.Join(flags, " "), nil
}

// stageTemplates are the role directories of the stages in model order
// (bin/render ROLES: the default template name is the role's directory).
func stageTemplates() []string {
	var out []string
	for _, s := range model.Stages() {
		out = append(out, s.Template)
	}
	return out
}

// tree is the rendered output, built in memory before anything is written:
// slash paths relative to the out dir.
type tree struct {
	files map[string]file
}

type file struct {
	data []byte
	perm fs.FileMode
}

// copyDir copies every regular file under dir in src to dst.
func (t *tree) copyDir(src fs.FS, dir, dst string) error {
	n := 0
	err := fs.WalkDir(src, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		t.files[path.Join(dst, strings.TrimPrefix(p, dir+"/"))] = file{b, 0o644}
		n++
		return nil
	})
	if err != nil {
		return fmt.Errorf("render: copy %s: %w", dir, err)
	}
	if n == 0 {
		return fmt.Errorf("render: copy %s: no files", dir)
	}
	return nil
}

// Render is `render CONFIG_DIR OUT_DIR`, from the data files in src
// (agentfactory.Assets in the binary). It wipes outDir and writes:
// scion-templates/<name>/ for each stage from templates/<flavour>/<role>
// (name from [templates], else the role), scion-templates/default, systemd/,
// images/scion-agent (from images/claude), images/pi and config.toml; the
// config dir's rules.md is appended to agent-rules.md, the placeholders are
// filled in the templates, the units, agent-rules.md and
// managed-settings.json, and the rendered rules are copied into images/pi.
//
// Unlike bin/render it checks everything before it touches outDir, so a
// refusal leaves the old output as it was.
func Render(src fs.FS, configDir, outDir string) error {
	cfgPath := filepath.Join(configDir, "config.toml")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	st, err := os.Stat(cfgPath)
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		return fmt.Errorf("render: %s: %w", cfgPath, err)
	}
	p := newPatterns()
	vs, err := values(cfg, p)
	if err != nil {
		return err
	}
	if err := checkOut(configDir, outDir); err != nil {
		return err
	}
	flavour := cfg.TemplateFlavour
	if !plainName(flavour) {
		return problemf("template_flavour: not a plain name: %s", pyRepr(flavour))
	}

	t := &tree{files: map[string]file{}}
	for _, s := range model.Stages() {
		name := cfg.Template(string(s.Name))
		if !plainName(name) {
			return problemf("[templates] %s: not a plain name: %s", s.Name, pyRepr(name))
		}
		if err := t.copyDir(src, "templates/"+flavour+"/"+s.Template, "scion-templates/"+name); err != nil {
			return err
		}
	}
	for _, c := range [][2]string{
		{"templates/default", "scion-templates/default"},
		{"systemd", "systemd"},
		{"images/claude", "images/scion-agent"},
		{"images/pi", "images/pi"},
	} {
		if err := t.copyDir(src, c[0], c[1]); err != nil {
			return err
		}
	}
	t.files["config.toml"] = file{raw, st.Mode().Perm()}

	const rules = "images/scion-agent/agent-rules.md"
	extra, err := os.ReadFile(filepath.Join(configDir, "rules.md"))
	switch {
	case err == nil:
		text, err := pyText(extra)
		if err != nil {
			return fmt.Errorf("render: %s: %w", filepath.Join(configDir, "rules.md"), err)
		}
		r := t.files[rules]
		r.data = append(append(append([]byte{}, r.data...), '\n'), text...)
		t.files[rules] = r
	case !os.IsNotExist(err):
		return fmt.Errorf("render: %w", err)
	}

	var targets []string
	for name := range t.files {
		if strings.HasPrefix(name, "scion-templates/") || strings.HasPrefix(name, "systemd/") {
			targets = append(targets, name)
		}
	}
	sort.Strings(targets)
	targets = append(targets, rules, "images/scion-agent/managed-settings.json")
	for _, name := range targets {
		f, ok := t.files[name]
		if !ok {
			return fmt.Errorf("render: %s: not in the core data", name)
		}
		out, err := fill(f.data, vs, p)
		if err != nil {
			return fmt.Errorf("%s: %w", pyJoin(outDir, name), err)
		}
		f.data = out
		t.files[name] = f
	}
	t.files["images/pi/agent-rules.md"] = file{t.files[rules].data, t.files[rules].perm}

	return t.write(outDir)
}

// fill is render_file: the text read as Python reads it in text mode
// (UTF-8, universal newlines), every {{name}} replaced; an unknown name is
// an error naming them all, sorted.
func fill(data []byte, vs ValueList, p patterns) ([]byte, error) {
	text, err := pyText(data)
	if err != nil {
		return nil, err
	}
	known := map[string]string{}
	for _, v := range vs {
		known[v.Name] = v.Text
	}
	unknown := map[string]bool{}
	for _, m := range p.placeholder.FindAllStringSubmatch(text, -1) {
		if _, ok := known[m[1]]; !ok {
			unknown[m[1]] = true
		}
	}
	if len(unknown) > 0 {
		names := make([]string, 0, len(unknown))
		for n := range unknown {
			names = append(names, pyRepr(n))
		}
		sort.Strings(names)
		return nil, problemf("unknown placeholders [%s]", strings.Join(names, ", "))
	}
	return []byte(substitute(p.placeholder, text, func(name string) string { return known[name] })), nil
}

// substitute is re.sub with a function of group 1.
func substitute(re *regexp.Regexp, text string, repl func(string) string) string {
	var b strings.Builder
	last := 0
	for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
		b.WriteString(text[last:m[0]])
		b.WriteString(repl(text[m[2]:m[3]]))
		last = m[1]
	}
	b.WriteString(text[last:])
	return b.String()
}

// pyText is a file's text as Python's open() in text mode gives it: valid
// UTF-8 required, and CRLF and lone CR read as LF.
func pyText(b []byte) (string, error) {
	if !utf8.Valid(b) {
		return "", Problem("not UTF-8 text")
	}
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	b = bytes.ReplaceAll(b, []byte("\r"), []byte("\n"))
	return string(b), nil
}

// pyJoin is os.path.join(out, rel) for the messages, which name the path
// as bin/render did.
func pyJoin(dir, rel string) string {
	if dir == "" || strings.HasSuffix(dir, "/") {
		return dir + rel
	}
	return dir + "/" + rel
}

// plainName is a single path element: a template directory or flavour
// cannot reach outside its parent.
func plainName(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00")
}

// checkOut refuses an out dir whose wipe would take the config dir with it
// (bin/render would delete config.toml before copying it) or the root.
func checkOut(configDir, outDir string) error {
	out, err := filepath.Abs(outDir)
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	cfg, err := filepath.Abs(configDir)
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	if r, err := filepath.EvalSymlinks(out); err == nil {
		out = r
	}
	if r, err := filepath.EvalSymlinks(cfg); err == nil {
		cfg = r
	}
	if out == "/" || out == cfg || strings.HasPrefix(cfg, out+"/") {
		return problemf("the out dir %s would be wiped, and it holds the config dir %s", outDir, configDir)
	}
	return nil
}

// write replaces outDir with the tree: directories 0755, each file with
// its mode exactly (shutil.copy2 and copytree copy the mode).
func (t *tree) write(outDir string) error {
	if err := fsx.RemoveTree(outDir); err != nil {
		return fmt.Errorf("render: %w", err)
	}
	names := make([]string, 0, len(t.files))
	for n := range t.files {
		names = append(names, n)
	}
	sort.Strings(names)
	made := map[string]bool{}
	mkdir := func(dir string) error {
		var chain []string
		for d := dir; d != "." && !made[d]; d = path.Dir(d) {
			chain = append(chain, d)
		}
		if err := os.MkdirAll(filepath.Join(outDir, filepath.FromSlash(dir)), 0o755); err != nil {
			return err
		}
		for _, d := range chain {
			made[d] = true
			if err := os.Chmod(filepath.Join(outDir, filepath.FromSlash(d)), 0o755); err != nil {
				return err
			}
		}
		return nil
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("render: %w", err)
	}
	if err := os.Chmod(outDir, 0o755); err != nil {
		return fmt.Errorf("render: %w", err)
	}
	for _, n := range names {
		f := t.files[n]
		if err := mkdir(path.Dir(n)); err != nil {
			return fmt.Errorf("render: %w", err)
		}
		dst := filepath.Join(outDir, filepath.FromSlash(n))
		if err := os.WriteFile(dst, f.data, f.perm); err != nil {
			return fmt.Errorf("render: %w", err)
		}
		if err := os.Chmod(dst, f.perm); err != nil {
			return fmt.Errorf("render: %w", err)
		}
	}
	return nil
}

// Assets is the embedded core data Render reads in the binary.
func Assets() fs.FS { return agentfactory.Assets }
