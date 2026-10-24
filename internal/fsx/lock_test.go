package fsx

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func lockPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "T0001")
}

func TestTryLockExcludesASecondOpenInTheSameProcess(t *testing.T) {
	path := lockPath(t)
	a, err := TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TryLock(path); !errors.Is(err, ErrBusy) {
		t.Fatalf("second TryLock: %v, want ErrBusy", err)
	}
	if err := a.Unlock(); err != nil {
		t.Fatal(err)
	}
	b, err := TryLock(path)
	if err != nil {
		t.Fatalf("after unlock: %v", err)
	}
	b.Unlock()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("lock files are never deleted: %v", err)
	}
}

// The lock half of TickRaceTest's CLI give-up (the busy refusal is store's).
func TestLockGivesUpAfterItsWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := lockPath(t)
		held, err := Lock(context.Background(), path, Forever)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Unlock()
		start := time.Now()
		_, err = Lock(context.Background(), path, Wait(10*time.Second))
		if !errors.Is(err, ErrBusy) {
			t.Fatalf("err %v, want ErrBusy", err)
		}
		if waited := time.Since(start); waited < 10*time.Second || waited > 10*time.Second+PollInterval {
			t.Errorf("gave up after %v, want 10s (polling every %v)", waited, PollInterval)
		}
		if _, err := Lock(context.Background(), path, NoWait); !errors.Is(err, ErrBusy) {
			t.Errorf("NoWait: %v", err)
		}
	})
}

func TestLockWaitsForTheHolder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := lockPath(t)
		held, err := Lock(context.Background(), path, Forever)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			time.Sleep(time.Minute)
			held.Unlock()
		}()
		start := time.Now()
		l, err := Lock(context.Background(), path, Forever)
		if err != nil {
			t.Fatal(err)
		}
		l.Unlock()
		if waited := time.Since(start); waited < time.Minute || waited > time.Minute+PollInterval {
			t.Errorf("got it after %v", waited)
		}
	})
}

func TestLockStopsWaitingWhenTheContextEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := lockPath(t)
		held, err := Lock(context.Background(), path, Forever)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := Lock(ctx, path, Forever); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err %v, want the context's", err)
		}
	})
}

func TestLockRefusesADeadContextUpFront(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Lock(ctx, lockPath(t), Forever); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}

// flock belongs to the open file description, so goroutines exclude each
// other exactly as processes do: never two holders at once.
func TestGoroutinesExcludeEachOther(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := lockPath(t)
		var inside, most atomic.Int32
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 5 {
					l, err := Lock(context.Background(), path, Forever)
					if err != nil {
						t.Error(err)
						return
					}
					n := inside.Add(1)
					if n > most.Load() {
						most.Store(n)
					}
					time.Sleep(time.Second)
					inside.Add(-1)
					l.Unlock()
				}
			}()
		}
		wg.Wait()
		if most.Load() != 1 {
			t.Errorf("%d holders at once", most.Load())
		}
	})
}

func TestLockDoesNotFollowALink(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "target"), filepath.Join(dir, "lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := TryLock(filepath.Join(dir, "lock")); err == nil {
		t.Fatal("locked through a link")
	}
	if _, err := os.Lstat(filepath.Join(dir, "target")); err == nil {
		t.Error("created the link's target")
	}
}

// helper processes: FSX_HELPER=hold locks the path in FSX_PATH, prints
// "locked" and holds it until stdin closes.
func TestHelperProcess(t *testing.T) {
	switch os.Getenv("FSX_HELPER") {
	case "":
		t.Skip("helper process only")
	case "hold":
		l, err := TryLock(os.Getenv("FSX_PATH"))
		if err != nil {
			fmt.Println("error", err)
			os.Exit(1)
		}
		fmt.Println("locked")
		io.Copy(io.Discard, os.Stdin)
		l.Unlock()
		os.Exit(0)
	}
}

type holder struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
}

func (h *holder) release(t *testing.T) {
	t.Helper()
	h.stdin.Close()
	if err := h.cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func startHolder(t *testing.T, cmd *exec.Cmd) *holder {
	t.Helper()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if strings.TrimSpace(line) != "locked" {
		stdin.Close()
		cmd.Wait()
		t.Fatalf("holder said %q (%v)", line, err)
	}
	h := &holder{cmd, stdin}
	t.Cleanup(func() { stdin.Close(); cmd.Wait() })
	return h
}

func goHolder(t *testing.T, path string) *holder {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), "FSX_HELPER=hold", "FSX_PATH="+path)
	return startHolder(t, cmd)
}

func TestProcessesExcludeEachOther(t *testing.T) {
	path := lockPath(t)
	h := goHolder(t, path)
	if _, err := Lock(context.Background(), path, Wait(300*time.Millisecond)); !errors.Is(err, ErrBusy) {
		t.Fatalf("while another process holds it: %v", err)
	}
	h.release(t)
	l, err := Lock(context.Background(), path, Wait(5*time.Second))
	if err != nil {
		t.Fatalf("after the other process let go: %v", err)
	}
	l.Unlock()
}

func TestWaitString(t *testing.T) {
	for w, s := range map[Wait]string{Forever: "forever", NoWait: "no wait", Wait(60 * time.Second): "1m0s"} {
		if w.String() != s {
			t.Errorf("%v: %q", strconv.Itoa(int(w)), w.String())
		}
	}
}
