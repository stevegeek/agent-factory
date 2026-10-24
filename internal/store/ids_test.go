package store

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// port: TF RobustnessTest.test_ticket_ids_never_collide_or_come_back
//
// The id lock is polled (fsx.Lock), so the contended waits run in a
// synctest bubble: the flocks are real, the 200 ms polls are not.
func TestTicketIDsNeverCollideOrComeBack(t *testing.T) {
	synctest.Test(t, ticketIDsNeverCollide)
}

func ticketIDsNeverCollide(t *testing.T) {
	f := newFixture(t)
	var mu sync.Mutex
	seen := map[ticket.ID]bool{}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8) // the Python test's 8 threads, 40 ids
	for range 40 {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			id, err := f.b.NextID(t.Context())
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			seen[id] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != 40 {
		t.Fatalf("%d distinct ids of 40", len(seen))
	}
	a := f.file(t, "x", "ready")
	os.Remove(f.p.TicketFile(string(a.ID))) // a deleted ticket's number is not handed out again
	if b := f.file(t, "y", "ready"); b.ID == a.ID {
		t.Errorf("%s came back", a.ID)
	}
	noTemps(t, f.p.Tickets)
}

// The counter: max(.last-id, every T*.json number) + 1, written as plain
// digits; a missing or garbage counter counts as 0.
func TestNextIDCounter(t *testing.T) {
	for _, c := range []struct {
		counter string // "" = no file
		files   []string
		want    ticket.ID
	}{
		{"", nil, "T0001"},
		{"41", nil, "T0042"},
		{" 41\n", nil, "T0042"},
		{"junk", []string{"T0007.json"}, "T0008"},
		{"", []string{"T0007.json", "T0003.json"}, "T0008"},
		{"5", []string{"T0009.json"}, "T0010"},
		{"20", []string{"T0009.json"}, "T0021"},
		{"9999", nil, "T10000"},
		{"", []string{"T12.json", "Tx1x2.json", "T0001.json.4.tmp"}, "T0013"}, // re.sub(r"\D", "", name) of every T*.json
		{"", []string{"Tnone.json"}, "T0001"},                                 // Python's int("") would raise; no digits count as none
		{"-4", nil, "T0001"},                                                  // Python would hand out "T-003"; a negative counter counts as 0
	} {
		f := newFixture(t)
		if c.counter != "" {
			os.WriteFile(f.p.LastID, []byte(c.counter), 0o644)
		}
		for _, n := range c.files {
			os.WriteFile(filepath.Join(f.p.Tickets, n), []byte("{}"), 0o644)
		}
		id, err := f.b.NextID(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if id != c.want {
			t.Errorf("counter %q files %v: %s, want %s", c.counter, c.files, id, c.want)
		}
		if b, _ := os.ReadFile(f.p.LastID); string(b) != strconv.Itoa(c.want.Num()) {
			t.Errorf("counter file %q after %s", b, c.want)
		}
		if _, err := os.Stat(f.p.LastID + ".tmp"); err == nil {
			t.Error(".last-id.tmp left")
		}
	}
}

// NextID makes the board directory if it is missing, as next_id does.
func TestNextIDMakesTheBoard(t *testing.T) {
	f := newFixture(t)
	os.RemoveAll(f.p.Board)
	if id, err := f.b.NextID(t.Context()); err != nil || id != "T0001" {
		t.Fatalf("%s %v", id, err)
	}
}

// Processes allocating together (the tick, Foreman and the CLI) never get
// the same id: the counter is under a flock, not a mutex.
func TestTicketIDsNeverCollideAcrossProcesses(t *testing.T) {
	f := newFixture(t)
	const procs, each = 4, 15
	var cmds []*exec.Cmd
	var outs []*bufio.Scanner
	for range procs {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
		cmd.Env = append(f.env.Environ(), "STORE_HELPER=ids", "STORE_IDS=15", "STORE_ROOT="+f.env.Root, "STORE_HOME="+f.env.Home)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
		outs = append(outs, bufio.NewScanner(out))
	}
	seen := map[string]bool{}
	for i, s := range outs {
		n := 0
		for s.Scan() {
			line := s.Text()
			if !strings.HasPrefix(line, "T") {
				continue
			}
			if seen[line] {
				t.Errorf("%s handed out twice", line)
			}
			seen[line] = true
			n++
		}
		if err := cmds[i].Wait(); err != nil {
			t.Fatal(err)
		}
		if n != each {
			t.Errorf("process %d gave %d ids", i, n)
		}
	}
	if len(seen) != procs*each {
		t.Errorf("%d distinct ids", len(seen))
	}
}

// TestHelperProcess is a child for the cross-process tests:
// STORE_HELPER=ids allocates STORE_IDS ids and prints them.
func TestHelperProcess(t *testing.T) {
	switch os.Getenv("STORE_HELPER") {
	case "":
		t.Skip("helper process only")
	case "ids":
		p := paths.FromEnv(map[string]string{"FACTORY_ROOT": os.Getenv("STORE_ROOT"), "HOME": os.Getenv("STORE_HOME")})
		b := &Board{Reader: &boardread.Reader{Paths: p}, Paths: p}
		n := 0
		for _, c := range os.Getenv("STORE_IDS") {
			n = n*10 + int(c-'0')
		}
		w := bufio.NewWriter(os.Stdout)
		for range n {
			id, err := b.NextID(t.Context())
			if err != nil {
				w.WriteString("error " + err.Error() + "\n")
				break
			}
			w.WriteString(string(id) + "\n")
		}
		w.Flush()
		os.Exit(0)
	}
}
