package fizz

import (
	"html"
	"regexp"
	"strings"
)

var looksHTML = regexp.MustCompile(`(?i)<(p|div|br|ul|ol|li|h[1-6]|pre|code|strong|em|b|i|a|blockquote|figure|table|action-text-attachment)[\s>/]`)

// Rich text arrives as the CLI sends it: HTML (it converts Markdown first) or plain text.
// It is stored as HTML and never served to a browser as HTML: the web view shows
// PlainText(…, true), a Markdown rendering of it, through Foreman's escaping Markdown helper.

// RichText stores what a client sent: HTML as is, plain text escaped into paragraphs.
func RichText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || looksHTML.MatchString(s) {
		return s
	}
	var b strings.Builder
	for _, para := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n\n") {
		if para = strings.TrimSpace(para); para != "" {
			b.WriteString("<p>" + strings.ReplaceAll(html.EscapeString(para), "\n", "<br>") + "</p>")
		}
	}
	return b.String()
}

// PlainText strips HTML to text. With md, emphasis, code, links, headings and lists keep
// their Markdown form, for the web view.
func PlainText(src string, md bool) string {
	var b strings.Builder
	var href []string // open links' targets
	pre := false
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
		nl := func(n int) {
			s := b.String()
			have := len(s) - len(strings.TrimRight(s, "\n"))
			if b.Len() == 0 {
				return
			}
			for ; have < n; have++ {
				b.WriteByte('\n')
			}
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
			if md && !closing {
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
				if md {
					b.WriteString("```\n")
				}
			} else if md {
				nl(1)
				b.WriteString("```")
			}
			pre = !closing
			if closing {
				nl(2)
			}
		case "code":
			if md && !pre {
				b.WriteByte('`')
			}
		case "strong", "b":
			if md {
				b.WriteString("**")
			}
		case "em", "i":
			if md {
				b.WriteByte('_')
			}
		case "a":
			if !md {
				break
			}
			if closing {
				if n := len(href); n > 0 {
					if href[n-1] != "" {
						b.WriteString("](" + href[n-1] + ")")
					}
					href = href[:n-1]
				}
			} else {
				h := attr(tag, "href")
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
