package ticketapi

import "testing"

func TestCreateAndUpdateSetWhatATicketWaitsFor(t *testing.T) {
	f := verbFx(t)
	first := str(f.boardTicket(""), "id")
	second := f.boardTicket(`"after": ["` + first + `"]`)
	if got := enc(at(second, "after")); got != `["`+first+`"]` {
		t.Fatalf("after %s", got)
	}
	id := str(second, "id")
	if c := f.refused("update", `{"id": "`+first+`", "after": ["`+id+`"]}`, "example-agent"); c != "bad_request" {
		t.Errorf("a cycle: %s", c)
	}
	if c := f.refused("create", `{"project": "sandbox", "title": "x", "after": ["T9999"]}`, ""); c != "bad_request" {
		t.Errorf("an unknown ticket: %s", c)
	}
	out := f.ok("update", `{"id": "`+id+`", "after": []}`, "example-agent")
	if !at(out.Value(), "ticket", "after").IsNull() {
		t.Errorf("after %s once cleared", enc(at(out.Value(), "ticket", "after")))
	}
}

func TestOnlyAPersonSetsWhatAFactoryTicketWaitsFor(t *testing.T) {
	f := verbFx(t)
	first := f.factoryTicket("fac", "First", "cli", "triage")
	second := f.factoryTicket("fac", "Second", "cli", "triage")
	req := `{"id": "` + string(second.ID) + `", "after": ["` + string(first.ID) + `"]}`
	if c := f.refused("update", req, "example-agent"); c != "factory_ticket" {
		t.Errorf("an agent: %s", c)
	}
	if c := f.refused("update", `{"id": "`+string(second.ID)+`", "title": "x", "after": []}`, ""); c != "factory_ticket" {
		t.Errorf("a title with it: %s", c)
	}
	f.ok("update", req, "")
	if got := enc(at(f.load(string(second.ID)), "after")); got != `["`+string(first.ID)+`"]` {
		t.Errorf("after %s", got)
	}
}

// A board ticket that waits for an open one cannot be started; once that
// one is done, it can.
func TestClaimWaitsForWhatATicketWaitsFor(t *testing.T) {
	f := verbFx(t)
	first := str(f.boardTicket(""), "id")
	second := str(f.boardTicket(`"after": ["`+first+`"]`), "id")
	if c := f.refused("claim", `{"id": "`+second+`"}`, "example-agent"); c != "conflict" {
		t.Errorf("claimed while it waits: %s", c)
	}
	f.ok("claim", `{"id": "`+first+`"}`, "example-agent")
	f.ok("move", `{"id": "`+first+`", "to": "done", "result": "Shipped."}`, "example-agent")
	f.ok("claim", `{"id": "`+second+`"}`, "example-agent")
}
