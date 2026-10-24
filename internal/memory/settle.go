package memory

import (
	"context"
	"strconv"
	"strings"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/memstore"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Copy is a run's copy as read back: its files, and what was left out.
type Copy struct {
	Files   memstore.Files
	Skipped []string
}

// ReadTar reads a helper run's memory.tar in memory (read_memory_tar). An
// error means the run's notes are lost (the caller records it and Lost).
func ReadTar(data []byte) (*Copy, error) {
	files, skipped, err := memstore.ReadTar(data)
	if err != nil {
		return nil, err
	}
	return &Copy{Files: files, Skipped: skipped}, nil
}

// memoryRef is the run's memory, if it has some it has not settled.
func memoryRef(t *ticket.Ticket) (ticket.Agent, ticket.MemoryRef, bool) {
	a, ok := t.Agent.Get()
	if !ok {
		return a, ticket.MemoryRef{}, false
	}
	m, ok := a.Memory.Get()
	if !ok || m.Settled {
		return a, m, false
	}
	return a, m, true
}

// markSettled records on the ticket's agent that its memory is done with,
// first, so nothing settles it twice.
func markSettled(t *ticket.Ticket, a ticket.Agent, m ticket.MemoryRef) {
	m.Settled = true
	a.Memory = ticket.Some(m)
	t.Agent = ticket.Some(a)
}

// Settle is settle_memory (F:1326): after a run, its container gone, what
// it changed is committed on the commit it started from and merged into
// the store, and a memory event says so on the ticket. remote is a helper
// run's copy (from ReadTar); nil reads the local copy. It never fails: an
// error becomes an error event, because memory must not hold up a ticket.
// The local copy is always removed. Settling twice does nothing.
func (s *Service) Settle(ctx context.Context, cfg *config.Config, t *ticket.Ticket, remote *Copy) {
	a, m, ok := memoryRef(t)
	if !ok {
		return
	}
	markSettled(t, a, m)
	defer func() {
		if m.Dir != "" {
			_ = memstore.RemoveCopy(m.Dir)
		}
	}()
	if err := s.settle(ctx, cfg, t, a.Name, m, remote); err != nil {
		text := textx.Prefix(err.Error(), 250)
		ticket.Event(t, "error", "memory: "+text, "", ticket.Errored{Text: text}, s.Clock.Now())
	}
}

func (s *Service) settle(ctx context.Context, cfg *config.Config, t *ticket.Ticket, agent string, m ticket.MemoryRef, remote *Copy) error {
	repo, err := s.Store.Repo(t.Project, m.Role)
	if err != nil {
		return err
	}
	var c Copy
	if remote != nil {
		c = *remote
	} else {
		if m.Dir == "" {
			return nil
		}
		files, skipped, err := memstore.ReadCopy(m.Dir, true)
		if err != nil {
			return err
		}
		c = Copy{Files: files, Skipped: skipped}
	}
	known, err := repo.Listing(ctx, m.Base)
	if err != nil {
		return err
	}
	tree, err := repo.WriteTree(ctx, c.Files, known)
	if err != nil {
		return err
	}
	baseTree, err := repo.TreeOf(ctx, m.Base)
	if err != nil {
		return err
	}
	if tree == baseTree {
		return nil
	}
	message := string(t.ID) + " " + agent
	run, err := repo.CommitTree(ctx, tree, []string{m.Base}, message)
	if err != nil {
		return err
	}
	changed, err := repo.Changed(ctx, m.Base, run)
	if err != nil {
		return err
	}
	_, conflicted, err := repo.Land(ctx, m.Base, run, message)
	if err != nil {
		return err
	}
	short, err := repo.Short(ctx, run)
	if err != nil {
		return err
	}
	detail := m.Role + " memory " + short + ": " + strings.Join(changed[:min(6, len(changed))], ", ")
	if len(changed) > 6 {
		detail += " …"
	}
	var rules []string
	// what persists past this ticket is where an injection would try to live
	if door, ok := model.DoorOf(t.Door); ok && door.FlagMemory {
		own := "tickets/" + string(t.ID) + ".md"
		for _, p := range changed {
			if p != own {
				rules = append(rules, "memory beyond this ticket's notes changed on a ticket from "+door.Label+": check it")
				break
			}
		}
	}
	if conflicted {
		rules = append(rules, "another run changed the same notes; both versions are kept in the file")
	}
	if n := len(c.Skipped); n > 0 {
		entries := "entries"
		if n == 1 {
			entries = "entry"
		}
		named := make([]string, 0, 3)
		for _, p := range c.Skipped[:min(3, n)] {
			named = append(named, textx.Prefix(memstore.PyRepr(p), 60))
		}
		rules = append(rules, "left out "+strconv.Itoa(n)+" "+entries+" (not plain files, odd names, or over the "+
			strconv.Itoa(memstore.MaxBytes/1024/1024)+" MB limit): "+strings.Join(named, ", "))
	}
	now := s.Clock.Now()
	ticket.Event(t, "memory", detail, strings.Join(rules, "; "), ticket.Memory{Role: m.Role, Commit: short, Files: changed}, now)
	if size := memstore.Size(c.Files); size > cfg.Memory.MaxKB*1024 {
		kb := size / 1024
		ticket.Event(t, "memory", m.Role+" memory is "+strconv.Itoa(kb)+" KB, over its limit: tidy it up", "",
			ticket.Memory{Role: m.Role, OverLimitKB: kb}, now)
	}
	return nil
}

// Lost is memory_lost (F:1318): a helper run that brought no usable copy
// back (timed out, lost, a failed collect, a broken tar). Its notes are
// gone; the ticket says so once.
func (s *Service) Lost(t *ticket.Ticket, host string) {
	a, m, ok := memoryRef(t)
	if !ok {
		return
	}
	markSettled(t, a, m)
	ticket.Event(t, "memory", "no memory came back from "+host+"; this run's notes are lost", "",
		ticket.Memory{Role: m.Role, LostOn: host}, s.Clock.Now())
}
