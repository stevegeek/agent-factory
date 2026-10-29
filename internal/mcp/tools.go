package mcp

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/stevegeek/agent-factory/internal/model"
)

// tool is one tool over one ticket verb.
type tool struct {
	name, title, desc string
	can               []string // every one needed for the tool to be listed
	write, idempotent bool
	schema            *jschema
	verb              string
	md                bool // ask the verb for Markdown
	// input is the verb's request from checked arguments; a message when
	// they don't fit together
	input func(args map[string]any) (map[string]any, string)
	reply func(out map[string]json.RawMessage) string
	hints map[string]string // by refusal code, over refusalHint
	args  map[string]string // the verb's field -> the tool's argument, where input renames one
}

func (t *tool) allowed(can []string) bool {
	for _, c := range t.can {
		if !slices.Contains(can, c) {
			return false
		}
	}
	return true
}

func (t *tool) describe(version string) map[string]any {
	out := map[string]any{"name": t.name, "description": t.desc, "inputSchema": t.schema.JSON()}
	if version == "2024-11-05" {
		return out // annotations came in 2025-03-26
	}
	if version != "2025-03-26" {
		out["title"] = t.title
	}
	ann := map[string]any{"title": t.title, "readOnlyHint": !t.write, "openWorldHint": false}
	if t.write {
		ann["destructiveHint"] = false
		ann["idempotentHint"] = t.idempotent
	}
	out["annotations"] = ann
	return out
}

// copyArgs is the arguments as they are, renamed where the verb calls them
// something else.
func copyArgs(rename map[string]string) func(map[string]any) (map[string]any, string) {
	return func(args map[string]any) (map[string]any, string) {
		in := map[string]any{}
		for k, v := range args {
			if n, ok := rename[k]; ok {
				k = n
			}
			in[k] = v
		}
		return in, ""
	}
}

func outString(out map[string]json.RawMessage, key string) string {
	var s string
	json.Unmarshal(out[key], &s)
	return s
}

type slimTicket struct {
	ID          string `json:"id"`
	Project     string `json:"project"`
	Column      string `json:"column"`
	ColumnLabel string `json:"column_label"`
}

func outTicket(out map[string]json.RawMessage, key string) slimTicket {
	var t slimTicket
	json.Unmarshal(out[key], &t)
	return t
}

func mdOf(out map[string]json.RawMessage) string { return outString(out, "markdown") }

// headed is a line of what happened, then the verb's Markdown.
func headed(head func(out map[string]json.RawMessage) string) func(map[string]json.RawMessage) string {
	return func(out map[string]json.RawMessage) string { return head(out) + "\n\n" + mdOf(out) }
}

func movedTo(out map[string]json.RawMessage) string {
	t := outTicket(out, "ticket")
	return fmt.Sprintf("%s is in %s.", t.ID, t.ColumnLabel)
}

// allColumns are the board columns, then the factory columns not among
// them: search_tickets' column enum.
func allColumns() []string {
	var out []string
	for _, c := range model.BoardColumns() {
		out = append(out, string(c.Name))
	}
	for _, c := range model.Columns() {
		if !slices.Contains(out, string(c.Name)) {
			out = append(out, string(c.Name))
		}
	}
	return out
}

// tools are the 13 tools, in the order tools/list shows them.
func tools() []*tool {
	return []*tool{
		{name: "my_work", title: "My work", can: []string{"read"}, idempotent: true, verb: "mine", md: true,
			desc: "What to do next, most urgent first: answers to your questions, questions put to you, your tickets in Doing and Next, unassigned tickets in Next and Inbox, then yours that wait. Call it first.",
			schema: sObj(map[string]*jschema{
				"project": sProject("Only this project"),
				"limit":   sInt(1, 50, "At most this many tickets in each group (default 20)"),
			}),
			input: copyArgs(nil), reply: mdOf},
		{name: "get_ticket", title: "Get a ticket", can: []string{"read"}, idempotent: true, verb: "get", md: true,
			desc:   "One ticket as Markdown: its body, questions and answers, conversation, links, and what it waits on.",
			schema: sObj(map[string]*jschema{"id": sID("The ticket, such as T0042")}, "id"),
			input:  copyArgs(nil), reply: mdOf},
		{name: "search_tickets", title: "Search tickets", can: []string{"read"}, idempotent: true, verb: "list", md: true,
			desc: "Tickets in your projects, newest first, one line each. Closed ones only with include_closed or a column. Ends with `More: cursor=…` when there is another page.",
			schema: sObj(map[string]*jschema{
				"project":        sProject("Only this project"),
				"column":         sEnum("Only this column", allColumns()...),
				"assignee":       sStr(64, `Only tickets assigned to this person or agent; "me" for yours`),
				"label":          &jschema{typ: "string", pattern: reLabel, desc: "Only tickets with this label"},
				"text":           sStr(200, "Every word appears in the title or body"),
				"updated_since":  &jschema{typ: "string", format: "date-time", desc: "Only tickets updated since (RFC 3339)"},
				"include_closed": sBool("Include Done, Dropped and Rejected tickets"),
				"limit":          sInt(1, 50, "At most this many (default 50)"),
				"cursor":         sID("The cursor from the last page's More line"),
			}),
			input: copyArgs(nil), reply: mdOf},
		{name: "file_ticket", title: "File a ticket", can: []string{"file"}, write: true, verb: "create", md: true,
			desc: "File a new ticket: into Inbox on a board project, or as a change into triage on a factory project. Give an idempotency_key derived from what you found, so a retry never files twice.",
			schema: sObj(map[string]*jschema{
				"project":         sProject("The project to file it on"),
				"title":           sStr(300, "A short title"),
				"body":            sStr(64*1024, "Markdown: what and why, and how to tell it is done"),
				"kind":            sEnum("item (the default on a board project), task (a question to settle) or change (the only kind on a factory project)", "item", "task", "change"),
				"labels":          sLabels("Lower-case labels"),
				"parent":          sID("File it under this ticket, on the same project"),
				"after":           sArr(sID(""), 0, 50, "Tickets that must be done before this one starts"),
				"idempotency_key": &jschema{typ: "string", pattern: reIdemKey, desc: "The same key with the same request answers the ticket it filed"},
			}, "project", "title"),
			input: copyArgs(nil),
			reply: headed(func(out map[string]json.RawMessage) string {
				if string(out["created"]) == "true" {
					return "Filed " + outTicket(out, "ticket").ID + "."
				}
				return "Already filed as " + outTicket(out, "ticket").ID + "."
			})},
		{name: "split_ticket", title: "Split a ticket", can: []string{"plan"}, write: true, verb: "split", md: true,
			desc: "Split a board ticket too large for one sitting into children filed under it.",
			schema: sObj(map[string]*jschema{
				"id": sID("The ticket to split"),
				"children": sArr(sObj(map[string]*jschema{
					"title":  sStr(300, "A short title"),
					"body":   sStr(64*1024, "Markdown"),
					"labels": sLabels("Lower-case labels"),
				}, "title"), 1, 20, "The children, in order"),
				"idempotency_key": &jschema{typ: "string", pattern: reSplitKey,
					desc: "The same key with the same children answers the children it filed"},
			}, "id", "children"),
			input: copyArgs(nil),
			reply: headed(func(out map[string]json.RawMessage) string {
				var kids []slimTicket
				json.Unmarshal(out["children"], &kids)
				ids := make([]string, len(kids))
				for i, k := range kids {
					ids[i] = k.ID
				}
				return fmt.Sprintf("Split %s into %s.", outTicket(out, "ticket").ID, strings.Join(ids, ", "))
			})},
		{name: "update_ticket", title: "Update a ticket", can: []string{"plan"}, write: true, idempotent: true, verb: "update", md: true,
			desc: "Change a board ticket's title, body, labels, parent or assignee. Only what you name changes.",
			schema: sObj(map[string]*jschema{
				"id":            sID("The ticket"),
				"title":         sStr(300, "A new title"),
				"body":          sStr(64*1024, "A new body (Markdown), in full"),
				"add_labels":    sLabels("Labels to add"),
				"remove_labels": sLabels("Labels to take off"),
				"parent":        &jschema{typ: "string", pattern: reTicketID, nullable: true, desc: "A new parent on the same project, or null for none"},
				"assignee":      &jschema{typ: "string", maxLen: 64, nullable: true, desc: "A person or an agent on the project, or null for nobody"},
				"after":         sArr(sID(""), 0, 50, "The tickets that must be done before this one starts, in full; [] for none"),
			}, "id"),
			input: copyArgs(nil), reply: mdOf},
		{name: "start_work", title: "Start work", can: []string{"work"}, write: true, idempotent: true, verb: "claim", md: true,
			desc:   "Claim a board ticket in Inbox or Next: it becomes yours and moves to Doing. It also takes your ticket back to Doing from Waiting, when it waits on something other than a question. Calling it again on your own ticket is fine.",
			schema: sObj(map[string]*jschema{"id": sID("The ticket")}, "id"),
			input:  copyArgs(nil),
			reply:  headed(func(out map[string]json.RawMessage) string { return "You have " + outTicket(out, "ticket").ID + "." }),
			hints:  map[string]string{"conflict": "get_ticket to see who has it and where it is; if it is not yours to take, pick another with my_work."}},
		{name: "post_update", title: "Post an update", can: []string{"talk"}, write: true, verb: "note", md: true,
			desc:   "Post a comment on a ticket: at real milestones, one line on what changed and what is next. On a factory ticket this is the only way to say something; the maintainer reads it on the ticket's page.",
			schema: sObj(map[string]*jschema{"id": sID("The ticket"), "text": sStr(16*1024, "Markdown")}, "id", "text"),
			input:  copyArgs(nil),
			reply:  headed(func(out map[string]json.RawMessage) string { return "Posted on " + outTicket(out, "ticket").ID + "." })},
		{name: "finish_work", title: "Finish work", can: []string{"work"}, write: true, idempotent: true, verb: "move", md: true,
			desc: "Finish a board ticket with a result a person can check: what changed, where (links, PR), what you didn't do. Outcome done moves it to Done; outcome waiting posts the result as a comment and moves it to Waiting with waiting_reason, for when you are blocked on something that is not a question.",
			schema: sObj(map[string]*jschema{
				"id":             sID("The ticket"),
				"result":         sStr(64*1024, "Markdown; at most 16 KiB with outcome waiting"),
				"outcome":        sEnum("done (the default) or waiting", "done", "waiting"),
				"waiting_reason": sStr(500, "With outcome waiting: what it waits on"),
			}, "id", "result"),
			input: func(args map[string]any) (map[string]any, string) {
				to, _ := args["outcome"].(string)
				if to == "" {
					to = "done"
				}
				in := map[string]any{"id": args["id"], "to": to, "result": args["result"]}
				r, ok := args["waiting_reason"]
				switch {
				case ok && to != "waiting":
					return nil, "waiting_reason goes with outcome waiting"
				case !ok && to == "waiting":
					return nil, "waiting_reason is needed with outcome waiting"
				case ok:
					in["reason"] = r
				}
				return in, ""
			},
			args:  map[string]string{"reason": "waiting_reason", "to": "outcome"},
			hints: map[string]string{"conflict": "get_ticket to see where it stands now. To add to a result already given, post_update."},
			reply: headed(movedTo)},
		{name: "drop_ticket", title: "Drop a ticket", can: []string{"work"}, write: true, idempotent: true, verb: "move", md: true,
			desc:   "Drop a board ticket that won't be done, saying why.",
			schema: sObj(map[string]*jschema{"id": sID("The ticket"), "reason": sStr(500, "Why it is dropped")}, "id", "reason"),
			input: func(args map[string]any) (map[string]any, string) {
				return map[string]any{"id": args["id"], "to": "dropped", "reason": args["reason"]}, ""
			},
			reply: headed(movedTo)},
		{name: "ask_person", title: "Ask a person", can: []string{"talk"}, write: true, verb: "ask",
			desc: "Ask a question on a board ticket instead of guessing, when the choice is a person's (scope, taste, anything irreversible). The ticket waits; the answer shows first in my_work and in get_ticket. Don't wait in a loop: pick other work.",
			schema: sObj(map[string]*jschema{
				"id":       sID("The ticket"),
				"question": sStr(4*1024, "One short question"),
				"options":  sArr(sStr(100, ""), 0, 6, "2 to 4 answers to choose from, when there are some"),
				"to":       sStr(64, "A person or an agent on the project (default: the maintainer)"),
			}, "id", "question"),
			input: copyArgs(map[string]string{"to": "recipient"}),
			args:  map[string]string{"recipient": "to"},
			reply: func(out map[string]json.RawMessage) string {
				var n int
				json.Unmarshal(out["n"], &n)
				id := outTicket(out, "ticket").ID
				return fmt.Sprintf("Asked %s (question %d). %s waits; the answer shows in `my_work` and `get_ticket`.\n",
					outString(out, "recipient"), n, id)
			}},
		{name: "answer_question", title: "Answer a question", can: []string{"talk"}, write: true, verb: "answer", md: true,
			desc: "Answer a question put to you (my_work lists them under Asked of you; get_ticket shows each question's number).",
			schema: sObj(map[string]*jschema{
				"id":       sID("The ticket"),
				"question": sInt(1, 1000, "The question's number"),
				"answer":   sStr(16*1024, "Markdown"),
			}, "id", "question", "answer"),
			input: copyArgs(map[string]string{"question": "n"}),
			args:  map[string]string{"n": "question"},
			reply: headed(func(out map[string]json.RawMessage) string { return "Answered on " + outTicket(out, "ticket").ID + "." })},
		{name: "send_to_factory", title: "Send to the factory", can: []string{"work", "file"}, write: true, verb: "send",
			desc: "Hand a board ticket's change to a factory project's pipeline: it is filed there as a change in triage, a person approves it, and the two tickets are linked. The board ticket stays where it is.",
			schema: sObj(map[string]*jschema{
				"id":      sID("The board ticket"),
				"project": sProject("The factory project"),
				"note":    sStr(4*1024, "A note for the factory's triage"),
			}, "id", "project"),
			input: copyArgs(nil),
			reply: func(out map[string]json.RawMessage) string {
				f := outTicket(out, "factory_ticket")
				return fmt.Sprintf("Sent: factory ticket %s in %s on %s; linked from %s.\n", f.ID, model.ColumnLabel(f.Column), f.Project,
					outTicket(out, "ticket").ID)
			}},
	}
}
