package textx

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// regex.json (tools/goldens.py) runs every regex the Python code uses over a
// corpus with non-ASCII letters and digits, NEL, LS, PS, \x1c-\x1f and
// trailing newlines. This test is the §8.5 checklist applied to every one of
// them: each Go translation below is built from the shared classes and
// helpers and must give Python's result for every case. The slices that own
// these patterns copy the translations from here.

type regexCase struct {
	In  string          `json:"in"`
	Out json.RawMessage `json:"out"`
}

type regexPattern struct {
	Name    string      `json:"name"`
	Where   string      `json:"where"`
	Pattern string      `json:"pattern"`
	Py      string      `json:"py"`
	Flags   []string    `json:"flags"`
	Op      string      `json:"op"`
	Repl    *string     `json:"repl"`
	Cases   []regexCase `json:"cases"`
}

type regexFile struct {
	Patterns []regexPattern `json:"patterns"`
}

func regexGolden(t *testing.T) regexFile {
	t.Helper()
	var g regexFile
	loadJSON(t, "regex.json", &g)
	if len(g.Patterns) == 0 {
		t.Fatal("no patterns in regex.json")
	}
	return g
}

// groups turns a submatch index slice into Python's [group0, group1, ...]
// with None (nil) for a group that did not take part.
func groups(s string, loc []int) any {
	if loc == nil {
		return nil
	}
	out := make([]any, len(loc)/2)
	for i := range out {
		if loc[2*i] >= 0 {
			out[i] = s[loc[2*i]:loc[2*i+1]]
		}
	}
	return out
}

func search(re *regexp.Regexp) func(string) any {
	return func(s string) any { return groups(s, re.FindStringSubmatchIndex(s)) }
}

// match is re.match: a leftmost-first search whose match starts at 0.
func match(re *regexp.Regexp) func(string) any {
	return func(s string) any {
		loc := re.FindStringSubmatchIndex(s)
		if loc == nil || loc[0] != 0 {
			return nil
		}
		return groups(s, loc)
	}
}

// fullmatch is re.fullmatch: the pattern wrapped in \A(?:...)\z.
func fullmatch(p string) func(string) any {
	return search(regexp.MustCompile(`\A(?:` + p + `)\z`))
}

// findall is re.findall: whole matches with no groups, group 1 with one,
// tuples with more ("" for a group that did not take part).
func findall(re *regexp.Regexp) func(string) any {
	return func(s string) any {
		out := []any{}
		for _, loc := range re.FindAllStringSubmatchIndex(s, -1) {
			g := func(i int) string {
				if loc[2*i] < 0 {
					return ""
				}
				return s[loc[2*i]:loc[2*i+1]]
			}
			switch n := re.NumSubexp(); n {
			case 0:
				out = append(out, g(0))
			case 1:
				out = append(out, g(1))
			default:
				tuple := make([]any, n)
				for i := range tuple {
					tuple[i] = g(i + 1)
				}
				out = append(out, tuple)
			}
		}
		return out
	}
}

func sub(re *regexp.Regexp, repl string) func(string) any {
	return func(s string) any {
		out, err := Sub(re, s, repl)
		if err != nil {
			return "error: " + err.Error()
		}
		return out
	}
}

func split(re *regexp.Regexp) func(string) any {
	return func(s string) any { return Split(re, s) }
}

// translations is the Go port of every pattern in regex.json: the RE2
// pattern (from the shared classes) and the operation the Python code applies.
func translations() map[string]func(string) any {
	d, w, sp := PyDigit, PyWord, PySpaceSet
	limit := regexp.MustCompile(`(?i)(` + FoldI(`usage limit reached|limit reached`+PyNotWord+`+resets?(?:(`+PyNotWord+`)|\z)|your limit will reset|you've hit your (usage )?limit|out of extra usage|credit balance is too low|Claude AI usage limit|(weekly|5-hour|session) limit reached`) + `)`)
	tier2 := regexp.MustCompile(`\A(eval|instance_eval|class_eval|send|public_send|constantize|system|exec|spawn|Marshal|YAML\.(unsafe_)?load|JSON\.parse|open\(|html_safe|raw\(|sanitize|find_by_sql|to_sql|Regexp\.new|Kernel\.|Process\.(run|exec|new)|from_json|from_yaml|YAML\.parse|pointerof|Pointer|unsafe_as|\.unsafe_)`)
	placeholder := regexp.MustCompile(`\{\{([a-z_]+)\}\}`)
	atEnd := func(p string) func(string) any {
		re := regexp.MustCompile(p)
		return func(s string) any {
			loc := AtEnd(re, s)
			if loc == nil || loc[0] != 0 {
				return nil
			}
			return groups(s, loc)
		}
	}
	return map[string]func(string) any{
		// \b at the start: a Unicode word boundary, checked by hand.
		"tier2_content": func(s string) any { return groups(s, SearchAtBoundary(tier2, s)) },
		// re.I: Python also matches İ and ı for an ASCII i (FoldI).
		"tier2_path":          search(regexp.MustCompile(`(?i)(` + FoldI(`pars|render|serial|encod|decod|request|response|http|json|yaml|sql|html|escap|input`) + `)`)),
		"ticket_id_fullmatch": fullmatch(`T` + d + `{4,6}`),
		"not_digit_sub":       sub(regexp.MustCompile(PyNotDigit), ""),
		"sentence_split":      func(s string) any { return SentenceSplit(s) },
		// $ (no re.M) with re.match: \z, and AtEnd also tries s without a final "\n".
		"repo_config_ok": atEnd(`^(core\.(repositoryformatversion|filemode|bare|logallrefupdates)|remote\.origin\.fetch|remote\.origin\.url|branch\..+\.(remote|merge)|extensions\.relativeworktrees)\z`),
		"slugify_sub":    sub(regexp.MustCompile(`[^a-z0-9]+`), "-"),
		// \b after resets?: an extra group stands in for the lookahead and Lookahead cuts it off.
		"limit_text": func(s string) any {
			loc := limit.FindStringSubmatchIndex(s)
			if loc == nil {
				return nil
			}
			return groups(s, Lookahead(loc, 2))
		},
		"memory_key":      atEnd(`^[A-Za-z0-9][A-Za-z0-9._-]{0,79}\z`),
		"version_digits":  findall(regexp.MustCompile(d + `+`)),
		"token_name":      fullmatch(`^[a-z][a-z0-9-]{0,31}\z`), // under fullmatch, $ is \z
		"label":           fullmatch(`^[a-z0-9][a-z0-9-]{0,31}\z`),
		"idempotency_key": fullmatch(`^[A-Za-z0-9._:-]{1,128}\z`),
		"ticket_id":       fullmatch(`^T` + d + `{4,6}\z`),
		"link_url":        fullmatch(`https?://[^` + sp + `<>"]+`),
		"backticks":       findall(regexp.MustCompile("`+")),
		"line_break":      search(regexp.MustCompile(`[\r\n\v\f\x{85}\x{2028}\x{2029}]`)),
		"scion_noise":     match(regexp.MustCompile(`^` + d + `{4}/` + d + d + `/` + d + d + ` ` + d + d + `:` + d + d + `:` + d + d + ` WARN `)),
		// re.S: (?s).
		"move_detail":            fullmatch(`(?s)(` + w + `+) -> (` + w + `+)(?:: (.*))?`),
		"index_name":             fullmatch(`(T` + d + `{4,6})\.json`),
		"job_name":               atEnd(`^[a-z0-9][a-z0-9-]{0,40}\z`),
		"branch_name":            atEnd(`^[A-Za-z0-9._/-]{1,100}\z`),
		"model_name":             atEnd(`^[A-Za-z0-9._:/-]{1,80}\z`),
		"render_placeholder":     findall(placeholder),
		"render_placeholder_sub": sub(placeholder, `<\1>`),
		"render_project":         fullmatch(`[A-Za-z0-9_.-]+`),
		"render_host":            fullmatch(`[A-Za-z0-9.-]+`),
		"render_login":           fullmatch(`[^` + sp + `"'\\%$]+`),
	}
}

func normJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEveryPythonRegexHasAGoTranslationThatAgrees(t *testing.T) {
	tr := translations()
	seen := map[string]bool{}
	for _, p := range regexGolden(t).Patterns {
		seen[p.Name] = true
		fn, ok := tr[p.Name]
		if !ok {
			t.Errorf("%s (%s): no Go translation", p.Name, p.Where)
			continue
		}
		for _, c := range p.Cases {
			var want any
			if err := json.Unmarshal(c.Out, &want); err != nil {
				t.Fatal(err)
			}
			if got, w := normJSON(t, fn(c.In)), normJSON(t, want); got != w {
				t.Errorf("%s %s(%q): got %s, Python %s", p.Name, p.Op, c.In, got, w)
			}
		}
	}
	for name := range tr {
		if !seen[name] {
			t.Errorf("translation %s has no pattern in regex.json", name)
		}
	}
}

func TestSubUsesPythonTemplates(t *testing.T) {
	re := regexp.MustCompile(`(a)(?P<n>b)?`)
	for _, c := range []struct{ in, repl, want string }{
		{"ab", `<\1>`, "<a>"},
		{"ab", `<\g<1>\g<n>>`, "<ab>"},
		{"a", `[\2]`, "[]"}, // a group that did not take part is empty
		{"ab", `$1\\`, `$1\`},
		{"ab", `\n\t`, "\n\t"},
		{"xaby", `-`, "x-y"},
		{"ab", `\g<0>!`, "ab!"},
		{"ab", `\012`, "\n"}, // three octal digits are a character
		{"ab", `\-`, `\-`},   // an escape of a non-letter stays as written
	} {
		got, err := Sub(re, c.in, c.repl)
		if err != nil || got != c.want {
			t.Errorf("Sub(%q, %q) = %q, %v, want %q", c.in, c.repl, got, err, c.want)
		}
	}
	// Python refuses these when it compiles the template.
	for _, repl := range []string{`\10`, `\g<x>`, `\g<9>`, `\q`, `x\`, `\g<1`, `\3`} {
		if got, err := Sub(re, "ab", repl); err == nil {
			t.Errorf("Sub(%q) = %q, want an error", repl, got)
		}
	}
}

func TestSplitKeepsGroupsLikePython(t *testing.T) {
	for _, c := range []struct {
		pat, in string
		want    []string
	}{
		{`,`, "a,b,,c", []string{"a", "b", "", "c"}},
		{`(,)`, "a,b", []string{"a", ",", "b"}},
		{`(,)|(;)`, "a,b;c", []string{"a", ",", "", "b", "", ";", "c"}},
		{`x`, "", []string{""}},
		{`x`, "x", []string{"", ""}},
	} {
		if got := Split(regexp.MustCompile(c.pat), c.in); strings.Join(got, "|") != strings.Join(c.want, "|") || len(got) != len(c.want) {
			t.Errorf("Split(%q, %q) = %q, want %q", c.pat, c.in, got, c.want)
		}
	}
}

func TestFoldILeavesEscapesAndFlagsAlone(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`input`, `[iIİı]nput`},
		{`I`, `[iIİı]`},
		{`(?i)x`, `(?i)x`},
		{`\bi`, `\b[iIİı]`},
		{`[a-i]`, `[a-i]`},
		{`\p{Lu}i`, `\p{Lu}[iIİı]`},
		{`\x{69}`, `\x{69}`},
	} {
		if got := FoldI(c.in); got != c.want {
			t.Errorf("FoldI(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
