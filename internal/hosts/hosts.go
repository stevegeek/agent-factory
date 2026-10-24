// Package hosts is host routing and the rules around usage limits
// (ARCHITECTURE.md §2.3, inventory-pipeline §14-15): which host runs a
// stage, which model it uses there, whether an agent's output says it hit
// a usage limit, and where a stopped run goes back to. It is pure: the
// busy counts, the paused hosts and the config are passed in.
package hosts

import (
	"regexp"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Local is this host's name in routing, agent records and pauses.
const Local = model.LocalHost

const (
	// PauseFor is LIMIT_PAUSE_MINUTES: how long a host that hit a usage
	// limit gets no new agents.
	PauseFor = 30 * time.Minute
	// QuietFor is QUIET_MINUTES: a local agent is checked for a limit only
	// once its activity log has been still this long.
	QuietFor = 4 * time.Minute
	// DefaultLimitLines is hit_limit's default: the last 15 lines count.
	DefaultLimitLines = 15
	// RemoteLimitLines and RemoteLimitChars are what a helper job's
	// agent.log is searched over (poll_remote, F:984): its last 20 000
	// characters, then the last 40 lines.
	RemoteLimitLines = 40
	RemoteLimitChars = 20_000
	// LookLimitLines is the screen a quiet local agent is looked at with.
	LookLimitLines = 40
)

// Pick is pick_host (F:836): the host that runs stage now, or false to
// wait. A host is free while its busy count is under its capacity:
// max_agents here, slots on a helper.
//   - A ticket pinned to a host runs only there: the pinned host if it is
//     known (this host only when localOK), not down and free.
//   - Otherwise, in order: a helper that prefers the stage; this host, when
//     localOK; a helper that lists the stage in stages or prefer. Each only
//     when not down and free. Helpers are tried in config order.
//
// localOK is false for a task with no project checkout here.
func Pick(cfg *config.Config, stage string, busy map[string]int, down map[string]bool, pinned string, localOK bool) (string, bool) {
	free := func(name string) bool {
		capacity := cfg.MaxAgents
		if name != Local {
			h, ok := cfg.HostNamed(name)
			if !ok {
				return false
			}
			capacity = h.Slots
		}
		return busy[name] < capacity
	}
	if pinned != "" {
		_, helper := cfg.HostNamed(pinned)
		known := pinned == Local && localOK || helper
		if known && !down[pinned] && free(pinned) {
			return pinned, true
		}
		return "", false
	}
	for _, h := range cfg.Hosts {
		if !down[h.Name] && contains(h.Prefer, stage) && free(h.Name) {
			return h.Name, true
		}
	}
	if localOK && !down[Local] && free(Local) {
		return Local, true
	}
	for _, h := range cfg.Hosts {
		if !down[h.Name] && (contains(h.Stages, stage) || contains(h.Prefer, stage)) && free(h.Name) {
			return h.Name, true
		}
	}
	return "", false
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// Model is stage_model (F:1836): [panel] models gives each voter its own
// model in turn (by the ticket's vote starts so far); otherwise [models]
// per stage on any host, then a helper's own model. "" is the harness's
// default. An unknown helper has no model (Python would raise).
func Model(cfg *config.Config, t *ticket.Ticket, stage, host string) string {
	if voters := cfg.Panel.Models; stage == string(model.StageVote) && len(voters) > 0 {
		return voters[ticket.Starts(t, model.StageVote)%len(voters)]
	}
	m := cfg.Model(stage)
	if m == "" && host != Local {
		if h, ok := cfg.HostNamed(host); ok {
			m = h.Model
		}
	}
	return m
}

// Timeout is ticket_timeout (F:167): the ticket's timeout_minutes when set
// and not 0, else agent_timeout_minutes. It bounds local and helper runs
// alike, and is sent to helpers as meta.timeout_minutes.
func Timeout(cfg *config.Config, t *ticket.Ticket) time.Duration {
	if m, ok := t.TimeoutMinutes.Get(); ok && m != 0 {
		return time.Duration(m) * time.Minute
	}
	return time.Duration(cfg.AgentTimeoutMinutes) * time.Minute
}

// RetryColumn is RETRY_FOR_STAGE: the column a stage's ticket waits in
// before it starts, where a run stopped by a usage limit goes back to.
// An unknown stage goes back to ready.
func RetryColumn(stage string) string {
	if info, ok := model.StageInfoOf(model.Stage(stage)); ok && info.RetryColumn != "" {
		return string(info.RetryColumn)
	}
	return string(model.ColReady)
}

// HitLimit is hit_limit (F:769): the usage-limit message in the last lines
// of text (str.splitlines(), every Python line break), or false. Only the
// end counts: a message that has scrolled away is not the agent's state.
// lines <= 0 searches everything, as Python's [-0:] does.
func HitLimit(text string, lines int) (string, bool) {
	all := textx.SplitLines(text)
	if lines > 0 && len(all) > lines {
		all = all[len(all)-lines:]
	}
	return limitSearch(strings.Join(all, "\n"))
}

// LIMIT_TEXT with re.I. Python's \W is PyNotWord, and the \b after
// "resets?" is an extra group that Lookahead cuts off the match.
//
// py: (usage limit reached|limit reached\W+resets?\b|your limit will reset|you've hit your (usage )?limit|out of extra usage|credit balance is too low|Claude AI usage limit|(weekly|5-hour|session) limit reached)
var limitText = regexp.MustCompile(`(?i)(` + textx.FoldI(`usage limit reached|limit reached`+textx.PyNotWord+`+resets?(?:(`+textx.PyNotWord+`)|\z)|your limit will reset|you've hit your (usage )?limit|out of extra usage|credit balance is too low|Claude AI usage limit|(weekly|5-hour|session) limit reached`) + `)`)

// limitSearch is LIMIT_TEXT.search(s).group(0).
func limitSearch(s string) (string, bool) {
	loc := textx.Lookahead(limitText.FindStringSubmatchIndex(s), 2)
	if loc == nil {
		return "", false
	}
	return s[loc[0]:loc[1]], true
}

// Pinnable reports a host a ticket may be pinned to: this one or a
// configured helper.
func Pinnable(cfg *config.Config, host string) bool {
	_, ok := cfg.HostNamed(host)
	return host == Local || ok
}
