package fizzy

import (
	"html"
	"strings"
)

// PlainText is Fizzy's rich text (HTML) as Markdown-ish text: tags
// stripped, entities decoded, paragraphs, line breaks, lists, headings,
// code, emphasis and links kept in their Markdown form. It reads the HTML
// as text only; nothing it writes is ever served as HTML.
func PlainText(src string) string {
	var b strings.Builder
	var href []string // open links' targets
	pre := false
	nl := func(n int) {
		if b.Len() == 0 {
			return
		}
		s := b.String()
		for have := len(s) - len(strings.TrimRight(s, "\n")); have < n; have++ {
			b.WriteByte('\n')
		}
	}
	for len(src) > 0 {
		i := strings.IndexByte(src, '<')
		if i < 0 {
			i = len(src)
		}
		text := html.UnescapeString(src[:i])
		if !pre {
			text = collapse(text)
			if s := b.String(); s == "" || strings.HasSuffix(s, "\n") || strings.HasSuffix(s, " ") {
				text = strings.TrimLeft(text, " ")
			}
		}
		b.WriteString(text)
		if i == len(src) {
			break
		}
		j := strings.IndexByte(src[i:], '>')
		if j < 0 {
			break
		}
		tag := src[i+1 : i+j]
		src = src[i+j+1:]
		closing := strings.HasPrefix(tag, "/")
		name := strings.ToLower(strings.TrimLeft(tag, "/"))
		if k := strings.IndexAny(name, " \t\n/"); k >= 0 {
			name = name[:k]
		}
		switch name {
		case "br":
			b.WriteByte('\n')
		case "p", "div", "blockquote", "ul", "ol", "table", "figure":
			nl(2)
		case "tr":
			nl(1)
		case "h1", "h2", "h3", "h4", "h5", "h6":
			nl(2)
			if !closing {
				b.WriteString("## ")
			}
		case "li":
			nl(1)
			if !closing {
				b.WriteString("- ")
			}
		case "pre":
			if !closing {
				nl(2)
				b.WriteString("```\n")
			} else {
				nl(1)
				b.WriteString("```")
				nl(2)
			}
			pre = !closing
		case "code":
			if !pre {
				b.WriteByte('`')
			}
		case "strong", "b":
			b.WriteString("**")
		case "em", "i":
			b.WriteByte('_')
		case "a":
			if closing {
				if n := len(href); n > 0 {
					if href[n-1] != "" {
						b.WriteString("](" + href[n-1] + ")")
					}
					href = href[:n-1]
				}
			} else {
				h := attr(tag, "href")
				if !validURL(h) {
					h = ""
				}
				if h != "" {
					b.WriteByte('[')
				}
				href = append(href, h)
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// collapse turns each run of white space into one space, as HTML shows it.
func collapse(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if r == ' ' || r == '\n' || r == '\t' || r == '\r' {
			space = true
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	if space {
		b.WriteByte(' ')
	}
	return b.String()
}

// attr reads one attribute's value from a tag's source.
func attr(tag, name string) string {
	low := strings.ToLower(tag)
	i := strings.Index(low, " "+name+"=")
	if i < 0 {
		return ""
	}
	v := tag[i+len(name)+2:]
	if v == "" {
		return ""
	}
	if q := v[0]; q == '"' || q == '\'' {
		if k := strings.IndexByte(v[1:], q); k >= 0 {
			return html.UnescapeString(v[1 : k+1])
		}
		return ""
	}
	if k := strings.IndexAny(v, " \t\n"); k >= 0 {
		v = v[:k]
	}
	return html.UnescapeString(v)
}
