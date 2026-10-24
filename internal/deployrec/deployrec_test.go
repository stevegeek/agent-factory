package deployrec

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRequestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deploys", "request.json")
	if _, ok, err := TakeRequest(path); ok || err != nil {
		t.Fatalf("no request: ok %v, %v", ok, err)
	}
	r := Request{Rev: "main", By: "maintainer", Why: "merged #12", Ticket: "T0042", At: "2026-10-24T10:00:00Z"}
	if err := WriteRequest(path, r); err != nil {
		t.Fatal(err)
	}
	got, ok, err := TakeRequest(path)
	if err != nil || !ok || got != r {
		t.Fatalf("TakeRequest = %+v, %v, %v", got, ok, err)
	}
	if _, ok, err := TakeRequest(path); ok || err != nil {
		t.Errorf("a second take: ok %v, %v", ok, err)
	}
}

func TestANewerRequestReplacesTheOlder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request.json")
	if err := WriteRequest(path, Request{Rev: "main", By: "clock"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteRequest(path, Request{Rev: "abc123456789", By: "maintainer", Drill: true}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := TakeRequest(path)
	if err != nil || !ok || got != (Request{Rev: "abc123456789", By: "maintainer", Drill: true}) {
		t.Errorf("TakeRequest = %+v, %v, %v", got, ok, err)
	}
}

// A request written while the runner takes the one before it waits for
// the next take: the take renames before it reads, so it never removes a
// file it did not read.
func TestARequestWrittenDuringATakeWaits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request.json")
	first, second := Request{Rev: "main", By: "clock"}, Request{Rev: "abc123456789", By: "maintainer", Drill: true}
	if err := WriteRequest(path, first); err != nil {
		t.Fatal(err)
	}
	got, ok, err := takeRequest(path, func() {
		if err := WriteRequest(path, second); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil || !ok || got != first {
		t.Fatalf("first take = %+v, %v, %v", got, ok, err)
	}
	got, ok, err = TakeRequest(path)
	if err != nil || !ok || got != second {
		t.Fatalf("the request written during the take = %+v, %v, %v", got, ok, err)
	}
	if _, err := os.Lstat(path + ".taken"); !os.IsNotExist(err) {
		t.Errorf("the taken file is left: %v", err)
	}
}

// A take that died between its rename and its remove left request.json.taken:
// the next take reads it.
func TestATakenRequestLeftBehindIsTaken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request.json")
	r := Request{Rev: "main", By: "clock"}
	if err := WriteRequest(path+".taken", r); err != nil {
		t.Fatal(err)
	}
	got, ok, err := TakeRequest(path)
	if err != nil || !ok || got != r {
		t.Fatalf("TakeRequest = %+v, %v, %v", got, ok, err)
	}
	if _, ok, err := TakeRequest(path); ok || err != nil {
		t.Errorf("taken twice: ok %v, %v", ok, err)
	}
}

func TestABadRequestIsAnErrorAndRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request.json")
	os.WriteFile(path, []byte("{not json"), 0o600)
	if _, ok, err := TakeRequest(path); ok || err == nil {
		t.Errorf("a bad request: ok %v, %v", ok, err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("the bad request is still there: %v", err)
	}
}

func TestStateRoundTripAndMissingIsZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deploys", "state.json")
	s, err := ReadState(path)
	if err != nil || !reflect.DeepEqual(s, State{}) {
		t.Fatalf("missing state = %+v, %v", s, err)
	}
	want := State{Phase: PhaseDone, Step: "helpers", Rev: "main", Commit: "abc123456789", From: "0123456789ab",
		ConfigCommit: "fedcba987654", By: "maintainer", Started: "a", Finished: "b", Result: ResultOK,
		Log: "/l", Helpers: map[string]string{"helpera": "ok", "desk": "ssh: connect refused"}}
	if err := WriteState(path, want); err != nil {
		t.Fatal(err)
	}
	if s, err := ReadState(path); err != nil || !reflect.DeepEqual(s, want) {
		t.Errorf("ReadState = %+v, %v", s, err)
	}
	os.WriteFile(path, []byte("[]"), 0o600)
	if _, err := ReadState(path); err == nil {
		t.Error("a state that is not an object reads")
	}
}

func TestHoldSetReadCleared(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foreman-state", "deploy-hold.json")
	if _, held, err := ReadHold(path); held || err != nil {
		t.Fatalf("no file: held %v, %v", held, err)
	}
	h := Hold{By: "maintainer", At: "2026-10-24T10:00:00Z", Note: "a week away"}
	if err := SetHold(path, &h); err != nil {
		t.Fatal(err)
	}
	if got, held, err := ReadHold(path); !held || err != nil || got != h {
		t.Errorf("ReadHold = %+v, %v, %v", got, held, err)
	}
	if err := SetHold(path, nil); err != nil {
		t.Fatal(err)
	}
	if _, held, err := ReadHold(path); held || err != nil {
		t.Errorf("cleared: held %v, %v", held, err)
	}
	if err := SetHold(path, nil); err != nil {
		t.Errorf("clearing twice: %v", err)
	}
}

// A hold file that does not read still holds: holding is the safe side.
func TestABadHoldStillHolds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deploy-hold.json")
	os.WriteFile(path, []byte("nope"), 0o600)
	if _, held, err := ReadHold(path); !held || err == nil {
		t.Errorf("bad hold: held %v, %v", held, err)
	}
}

func TestReadRequestLeavesItWaiting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request.json")
	if _, ok, err := ReadRequest(path); ok || err != nil {
		t.Fatalf("no request: ok %v, %v", ok, err)
	}
	r := Request{Rev: "main", By: "maintainer", At: "2026-10-24T10:00:00Z"}
	if err := WriteRequest(path, r); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if got, ok, err := ReadRequest(path); err != nil || !ok || got != r {
			t.Fatalf("ReadRequest = %+v, %v, %v", got, ok, err)
		}
	}
	if _, ok, _ := TakeRequest(path); !ok {
		t.Error("the request is gone")
	}
}
