package main

import (
	"strings"
	"testing"
)

func TestMarkdownRendersWhatAgentsWrite(t *testing.T) {
	src := "## Plan\n\nMake **encoders** immutable; see `Hashid#encode` and [the issue](https://github.com/o/r/issues/15).\n\n" +
		"1. Freeze state\n2. Add tests\n   - threads\n   - deep freeze\n\n> a quote\n\n```ruby\nputs \"<hi>\"\n```\n\n" +
		"| Ticket | Effort |\n| --- | --- |\n| One | small |\n\n---\n\n*done* at https://example.com/x."
	got := string(Markdown(src))
	for _, want := range []string{
		"<h4>Plan</h4>",
		"<strong>encoders</strong>",
		"<code>Hashid#encode</code>",
		`<a href="https://github.com/o/r/issues/15" target="_blank" rel="noopener noreferrer" hx-boost="false">the issue</a>`,
		"<ol><li>Freeze state</li><li>Add tests<ul><li>threads</li><li>deep freeze</li></ul>",
		"<blockquote><p>a quote</p>",
		"<pre><code>puts &#34;&lt;hi&gt;&#34;</code></pre>",
		"<th>Ticket</th>", "<td>small</td>",
		"<hr>",
		"<em>done</em>",
		`<a href="https://example.com/x"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

func TestMarkdownIsSafe(t *testing.T) {
	got := string(Markdown("<script>alert(1)</script> [x](javascript:alert(1)) <img src=x onerror=alert(1)> `<b>` **<i>**\x00\x01"))
	for _, bad := range []string{"<script", "<img", "javascript:", "<b>", "<i>", "\x00", "\x01"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q got through: %s", bad, got)
		}
	}
	if !strings.Contains(got, "&lt;script&gt;") {
		t.Errorf("text should be escaped, not dropped: %s", got)
	}
}
