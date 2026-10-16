package main

// A small Markdown renderer for what agents write (designs, answers, proposals, ticket bodies).
// Safe by construction: every piece of text is HTML-escaped before any tag is added, and only
// http(s) links become links, because some of this text started in a public issue tracker.

import (
	"html"
	"html/template"
	"regexp"
	"strings"
)

var (
	mdHeading  = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
	mdBullet   = regexp.MustCompile(`^(\s*)([-*+])\s+(.*)$`)
	mdOrdered  = regexp.MustCompile(`^(\s*)(\d{1,9})[.)]\s+(.*)$`)
	mdRule     = regexp.MustCompile(`^\s*(?:(?:-\s*){3,}|(?:\*\s*){3,}|(?:_\s*){3,})$`)
	mdFence    = regexp.MustCompile("^\\s*(```|~~~)")
	mdTableSep = regexp.MustCompile(`^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$`)
	mdCode     = regexp.MustCompile("`([^`]+)`")
	mdLink     = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
	mdBare     = regexp.MustCompile(`(^|[\s(])(https?://[^\s<)]+[^\s<).,;:!?'"])`)
	mdBold     = regexp.MustCompile(`\*\*([^*]+?)\*\*|__([^_]+?)__`)
	mdItalic   = regexp.MustCompile(`(^|[^\w*])\*([^*\s][^*]*?)\*|(^|[^\w])_([^_\s][^_]*?)_($|[^\w])`)
)

// Markdown renders text as safe HTML.
func Markdown(src string) template.HTML {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	var b strings.Builder
	var para []string
	flush := func() {
		if len(para) > 0 {
			b.WriteString("<p>" + mdInline(strings.Join(para, " ")) + "</p>\n")
			para = nil
		}
	}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			flush()
		case mdFence.MatchString(line):
			flush()
			fence := strings.TrimSpace(line)[:3]
			var code []string
			for i++; i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), fence); i++ {
				code = append(code, lines[i])
			}
			b.WriteString("<pre><code>" + html.EscapeString(strings.Join(code, "\n")) + "</code></pre>\n")
		case mdHeading.MatchString(trimmed):
			flush()
			m := mdHeading.FindStringSubmatch(trimmed)
			// the page's own headings sit above these: start at h4
			tag := "h4"
			if len(m[1]) >= 3 {
				tag = "h5"
			}
			b.WriteString("<" + tag + ">" + mdInline(m[2]) + "</" + tag + ">\n")
		case mdRule.MatchString(line) && len(para) == 0:
			b.WriteString("<hr>\n")
		case strings.HasPrefix(trimmed, ">"):
			flush()
			var quote []string
			for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), ">"); i++ {
				quote = append(quote, strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(lines[i]), ">"), " "))
			}
			i--
			b.WriteString("<blockquote>" + string(Markdown(strings.Join(quote, "\n"))) + "</blockquote>\n")
		case strings.Contains(trimmed, "|") && i+1 < len(lines) && mdTableSep.MatchString(lines[i+1]):
			flush()
			b.WriteString("<div class=\"md-table\"><table><thead><tr>")
			for _, c := range mdCells(trimmed) {
				b.WriteString("<th>" + mdInline(c) + "</th>")
			}
			b.WriteString("</tr></thead><tbody>")
			for i += 2; i < len(lines) && strings.Contains(lines[i], "|") && strings.TrimSpace(lines[i]) != ""; i++ {
				b.WriteString("<tr>")
				for _, c := range mdCells(strings.TrimSpace(lines[i])) {
					b.WriteString("<td>" + mdInline(c) + "</td>")
				}
				b.WriteString("</tr>")
			}
			i--
			b.WriteString("</tbody></table></div>\n")
		case mdBullet.MatchString(line) || mdOrdered.MatchString(line):
			flush()
			i = mdList(lines, i, &b) - 1
		default:
			para = append(para, trimmed)
		}
	}
	flush()
	return template.HTML(b.String())
}

// mdList renders the list starting at lines[i] (nested by indentation) and returns the next line index.
func mdList(lines []string, i int, b *strings.Builder) int {
	indent := func(l string) int { return len(l) - len(strings.TrimLeft(l, " \t")) }
	base := indent(lines[i])
	ordered := mdOrdered.MatchString(lines[i]) && !mdBullet.MatchString(lines[i])
	tag := "ul"
	if ordered {
		tag = "ol"
	}
	b.WriteString("<" + tag + ">")
	open := false
	for i < len(lines) {
		l := lines[i]
		if strings.TrimSpace(l) == "" {
			// a blank line ends the list unless the next line carries on with it
			if i+1 < len(lines) && (indent(lines[i+1]) > base || mdBullet.MatchString(lines[i+1]) || mdOrdered.MatchString(lines[i+1])) && indent(lines[i+1]) >= base {
				i++
				continue
			}
			break
		}
		m := mdBullet.FindStringSubmatch(l)
		if m == nil {
			m = mdOrdered.FindStringSubmatch(l)
		}
		switch {
		case m != nil && indent(l) == base:
			if open {
				b.WriteString("</li>")
			}
			b.WriteString("<li>" + mdInline(m[3]))
			open = true
			i++
		case m != nil && indent(l) > base:
			i = mdList(lines, i, b)
		case m == nil && indent(l) > base && open:
			b.WriteString(" " + mdInline(strings.TrimSpace(l)))
			i++
		default:
			if open {
				b.WriteString("</li>")
			}
			b.WriteString("</" + tag + ">\n")
			return i
		}
	}
	if open {
		b.WriteString("</li>")
	}
	b.WriteString("</" + tag + ">\n")
	return i
}

func mdCells(row string) []string {
	row = strings.TrimSuffix(strings.TrimPrefix(row, "|"), "|")
	cells := strings.Split(row, "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

// mdInline: code spans are set aside first (their text stays literal), the rest is escaped, then marked up.
func mdInline(s string) string {
	// the placeholders below use these two bytes; text never carries them
	s = strings.NewReplacer("\x00", "", "\x01", "").Replace(s)
	var codes []string
	s = mdCode.ReplaceAllStringFunc(s, func(m string) string {
		codes = append(codes, "<code>"+html.EscapeString(m[1:len(m)-1])+"</code>")
		return "\x00" + string(rune('0'+len(codes)-1)) + "\x00"
	})
	s = html.EscapeString(s)
	var links []string
	s = mdLink.ReplaceAllStringFunc(s, func(m string) string {
		p := mdLink.FindStringSubmatch(m)
		u := safeURL(html.UnescapeString(p[2]))
		if u == "" {
			return p[1]
		}
		links = append(links, `<a href="`+html.EscapeString(u)+`" target="_blank" rel="noopener noreferrer" hx-boost="false">`+p[1]+`</a>`)
		return "\x01" + string(rune('0'+len(links)-1)) + "\x01"
	})
	s = mdBare.ReplaceAllStringFunc(s, func(m string) string {
		p := mdBare.FindStringSubmatch(m)
		u := safeURL(html.UnescapeString(p[2]))
		if u == "" {
			return m
		}
		return p[1] + `<a href="` + html.EscapeString(u) + `" target="_blank" rel="noopener noreferrer" hx-boost="false">` + p[2] + `</a>`
	})
	s = mdBold.ReplaceAllString(s, "<strong>$1$2</strong>")
	s = mdItalic.ReplaceAllString(s, "$1$3<em>$2$4</em>$5")
	for i, l := range links {
		s = strings.Replace(s, "\x01"+string(rune('0'+i))+"\x01", l, 1)
	}
	for i, c := range codes {
		s = strings.Replace(s, "\x00"+string(rune('0'+i))+"\x00", c, 1)
	}
	return s
}

var mdHeadMark = regexp.MustCompile(`(^|\s)#{1,6}\s+`)

// MarkdownLine renders one line of agent text (a title, a summary, a story step): inline marks only.
// Text cut down to a few sentences can carry heading marks from the middle of a document; they go.
func MarkdownLine(s string) template.HTML {
	return template.HTML(mdInline(mdHeadMark.ReplaceAllString(s, "$1")))
}

// mdLead: the opening of a Markdown document as one line, for a summary. It skips headings, rules,
// code and tables, and takes the first paragraph, or the first list items when the text opens with a list.
func mdLead(src string, n int) string {
	var out []string
	inFence, inList := false, false
	for _, line := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case mdFence.MatchString(line):
			inFence = !inFence
			continue
		case inFence:
			continue
		case t == "":
			if len(out) > 0 && !inList {
				return clip(strings.Join(out, " "), n)
			}
			continue
		case mdHeading.MatchString(t), mdRule.MatchString(line), strings.HasPrefix(t, "|"):
			if len(out) > 0 {
				return clip(strings.Join(out, " "), n)
			}
			continue
		}
		if m := mdBullet.FindStringSubmatch(line); m != nil {
			inList, t = true, m[3]+";"
		} else if m := mdOrdered.FindStringSubmatch(line); m != nil {
			inList, t = true, m[3]+";"
		} else if inList {
			return clip(strings.TrimSuffix(strings.Join(out, " "), ";"), n)
		}
		out = append(out, strings.TrimPrefix(t, "> "))
		if len(strings.Join(out, " ")) >= n {
			break
		}
	}
	return clip(strings.TrimSuffix(strings.Join(out, " "), ";"), n)
}

var mdAnchor = regexp.MustCompile(`<a [^>]*>|</a>`)

// MarkdownText: MarkdownLine without links, for text that sits inside a link already (a queue item, a card).
func MarkdownText(s string) template.HTML {
	return template.HTML(mdAnchor.ReplaceAllString(string(MarkdownLine(s)), ""))
}
