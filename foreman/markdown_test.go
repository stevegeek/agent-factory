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

func TestOneLineMarkdownAndTheLeadOfAnAnswer(t *testing.T) {
	if got := string(MarkdownLine("Make `require \"x\"` work ## 1. Branches **all** <b>")); got !=
		`Make <code>require &#34;x&#34;</code> work 1. Branches <strong>all</strong> &lt;b&gt;` {
		t.Errorf("line: %s", got)
	}
	doc := "## Hygiene check\n\n### 1. Branches\n\nAll three branches are **open**.\nLeave them.\n\n| a | b |\n| --- | --- |\n"
	if got := mdLead(doc, 320); got != "All three branches are **open**. Leave them." {
		t.Errorf("lead: %q", got)
	}
	if got := mdLead("# Plan\n\n- one\n- two\n\nlater", 320); got != "one; two" {
		t.Errorf("list lead: %q", got)
	}
}

func TestTextInsideALinkGetsNoLink(t *testing.T) {
	if got := string(MarkdownText("see https://example.com/x and `code`")); strings.Contains(got, "<a") || !strings.Contains(got, "<code>code</code>") {
		t.Errorf("text: %s", got)
	}
}
