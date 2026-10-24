package ticketapi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/jsonx"
)

// §6.4, §13.4: Foreman runs `list` text searches in-process, up to 16 MiB
// of reads per call, two at a time (--api-concurrency 2), under
// SetMemoryLimit(64 MiB) and MemoryMax=128M. The search reads one file at a
// time and never holds them together, so two concurrent searches over a
// 16 MiB board grow the heap by a few file sizes, not by the board: this
// soak runs them several rounds and bounds the live heap well below 64 MiB.
func TestTwoConcurrentSearchesStayWithinTheMemoryBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a 16 MiB board")
	}
	f := limitsFx(t)
	first := str(f.boardTicket(`"body": "seed"`), "id")
	seed, err := os.ReadFile(filepath.Join(f.board.Paths.Tickets, first+".json"))
	if err != nil {
		t.Fatal(err)
	}
	// 180 tickets of about 100 KiB each, written as files: past the 16 MiB a
	// search may read, so every call reads its whole budget
	const n, size = 180, 100 << 10
	body := strings.Repeat("lorem ipsum dolor sit amet ", size/27)
	for i := 2; i <= n+1; i++ {
		id := fmt.Sprintf("T%04d", i)
		tree, err := jsonx.Parse(seed)
		if err != nil {
			t.Fatal(err)
		}
		o, _ := tree.Object()
		o.Set("id", jsonx.String(id))
		o.Set("body", jsonx.String(body))
		if err := os.WriteFile(filepath.Join(f.board.Paths.Tickets, id+".json"), jsonx.EncodePython(o.Value(), 2), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(f.board.Paths.LastID, []byte(fmt.Sprint(n+1)), 0o644)
	f.index() // the index is built once, as it is on a live board

	search := func() error {
		out, err := f.svc.Do(context.Background(), callerOf("example-agent"), "list", parse(t, `{"text": "nowhere"}`), 30*time.Second)
		if err != nil {
			return err
		}
		if l := length(out.Get("tickets")); l != 0 {
			return fmt.Errorf("matched %d", l)
		}
		if out.Get("next").IsNull() {
			return fmt.Errorf("the search was not cut at its byte budget: it read less than 16 MiB")
		}
		return nil
	}
	if err := search(); err != nil { // warm: the first call's one-off allocations
		t.Fatal(err)
	}
	// Foreman's own settings (web.Main): GOGC 50 and a 64 MiB soft limit
	defer debug.SetGCPercent(debug.SetGCPercent(50))
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(64 << 20))
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)

	var peak atomic.Uint64
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			runtime.ReadMemStats(&m)
			for {
				p := peak.Load()
				if m.HeapInuse <= p || peak.CompareAndSwap(p, m.HeapInuse) {
					break
				}
			}
		}
	}()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 4 {
				if err := search(); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-sampled
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	growth := int64(peak.Load()) - int64(base.HeapInuse)
	t.Logf("heap in use: %d MiB before, peak %d MiB during two concurrent 16 MiB searches of a %d MiB board",
		base.HeapInuse>>20, peak.Load()>>20, (n*size)>>20)
	// A search holding the board would keep 16 MiB live per call, 32 MiB for
	// two, and push the heap past the limit; one reading a file at a time
	// stays at a few MiB (more on a loaded machine, where the collector lags
	// the allocations: the bound leaves room for that).
	if limit := int64(40 << 20); growth > limit {
		t.Errorf("the heap grew by %d MiB, more than %d MiB", growth>>20, limit>>20)
	}
}
