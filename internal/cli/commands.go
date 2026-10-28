package cli

import (
	"github.com/stevegeek/agent-factory/internal/ticketapi"
	"github.com/stevegeek/agent-factory/internal/usage"
)

// The command table: every command the factory has, with the argv
// Python's argparse took, unchanged. Each group's Run lives in its own
// cmd_<group>.go:
//
//	cmd_readonly.go, cmd_ticket.go: ls show result proposals recipes model check version ticket tokens
//	cmd_ops.go, cmd_inspect.go:     add move note reply ask approve redirect merge pr host migrate wake answer look audit memory github gate-workflow findings project
//	cmd_doors.go:                   sweep clock rebuild versions
//	cmd_tick.go:                    tick early-checks
//	cmd_render.go:                  render patch-scion
//	cmd_screens.go:                 screens
//	cmd_fizzy.go:                   fizzy (export, import)
//	cmd_onboard.go:                 kinds onboard
//	cmd_usage.go:                   usage
func commands() []Command {
	ticketArg := Positional{Name: "ticket"}
	return []Command{
		// the maintainer's verbs
		{Name: "add", Help: "file a ticket and print its id", Config: true,
			Positionals: []Positional{{Name: "project"}, {Name: "title", Optional: true}},
			Flags: []Flag{
				{Name: "recipe", Help: "file a recipe (see `factory recipes`); title and body default to the recipe's"},
				{Name: "body"},
				{Name: "body-file"},
				{Name: "door", Default: "human", Choices: []string{"human", "clock", "github", "peer"}},
				{Name: "triage", Kind: BoolFlag},
				{Name: "host", Help: "run every stage on this host (local or a [hosts] name); default auto"},
				{Name: "task", Kind: BoolFlag, Help: "one agent run, no branch/review/gate; the result comes back to you"},
				{Name: "timeout", Kind: IntFlag, Help: "minutes this ticket's agents may run (default agent_timeout_minutes)"},
				{Name: "intent", Kind: BoolFlag, Help: "an idea to design first: a designer proposes, you `factory approve` or `redirect`"},
				{Name: "manage", Kind: BoolFlag, Help: "a request for the manager: it answers and drafts config changes for you"},
				{Name: "findings", Kind: BoolFlag, Help: "with --task: it reports structured findings, which a verifier checks before you file them"},
			},
			Run: runAdd},
		{Name: "findings", Help: "list a check's verified findings, or file them as tickets", Config: true,
			Positionals: []Positional{ticketArg},
			Flags: []Flag{
				{Name: "file", Help: "comma-separated numbers of the findings to file, or all; the check closes"},
				{Name: "as", Help: "with --file: how to file some, e.g. 1=intent,3=change (default: intent when it changes behaviour)"},
				{Name: "none", Kind: BoolFlag, Help: "file none of them; the check closes"},
				{Name: "verify", Kind: BoolFlag, Help: "send a finished task's result to the verifier (for one whose findings are prose)"},
			},
			Run: runFindings},
		{Name: "result", Help: "print a task's result or an idea's design", Config: true,
			Positionals: []Positional{ticketArg}, Run: runResult},
		{Name: "approve", Help: "file an idea's proposed tickets; prints their ids", Config: true,
			Positionals: []Positional{ticketArg},
			Flags:       []Flag{{Name: "only", Help: "comma-separated numbers of the proposed tickets to file"}},
			Run:         runApprove},
		{Name: "redirect", Help: "send an idea back to its designer", Config: true,
			Positionals: []Positional{ticketArg},
			Flags:       []Flag{{Name: "note", Required: true, Help: "what to change in the design"}},
			Run:         runRedirect},
		{Name: "host", Help: "pin a ticket to a host", Config: true,
			Positionals: []Positional{ticketArg, {Name: "host", Help: "auto, local, or a [hosts] name"}},
			Run:         runHost},
		{Name: "ls", Help: "list the tickets", Config: true,
			Flags: []Flag{{Name: "all", Kind: BoolFlag, Help: "closed tickets too"}}, Run: runLs},
		{Name: "recipes", Help: "list the recipes", Config: true,
			Flags: []Flag{{Name: "json", Kind: BoolFlag}}, Run: runRecipes},
		{Name: "show", Help: "print a ticket's file", Config: true,
			Positionals: []Positional{ticketArg}, Run: runShow},
		{Name: "model", Help: "print the board's model as JSON", Run: runModel},
		{Name: "look", Help: "show what a ticket's agent shows now", Config: true,
			Positionals: []Positional{ticketArg},
			Flags:       []Flag{{Name: "lines", Kind: IntFlag, Default: "120"}},
			Run:         runLook},
		{Name: "audit", Help: "print a ticket's audit", Config: true,
			Positionals: []Positional{ticketArg}, Run: runAudit},
		{Name: "memory", Help: "show or undo the agents' memory", Config: true,
			Positionals: []Positional{{Name: "project", Optional: true}, {Name: "role", Optional: true}},
			Flags:       []Flag{{Name: "revert", Metavar: "COMMIT"}},
			Run:         runMemory},
		{Name: "proposals", Help: "list the open proposals by priority", Config: true, Run: runProposals},
		{Name: "pr", Help: "open or update a ticket's PR and print its URL", Config: true,
			Positionals: []Positional{ticketArg}, Run: runPR},
		{Name: "github", Help: "show the App's repositories and permissions", Config: true, Run: runGitHub},
		{Name: "prune-branches", Help: "list the bot/* branches on GitHub whose ticket is done with a merged PR", Config: true, Run: runPruneBranches,
			Flags: []Flag{{Name: "apply", Kind: BoolFlag, Help: "delete them (default: a dry run that deletes nothing)"}}},
		{Name: "gate-workflow", Help: "print a project's gate workflow", Config: true,
			Positionals: []Positional{{Name: "project"}}, Run: runGateWorkflow},
		{Name: "move", Help: "move a ticket to a column", Config: true,
			Positionals: []Positional{ticketArg, {Name: "column"}},
			Flags:       []Flag{{Name: "note"}},
			Run:         runMove},
		{Name: "note", Help: "add a note to a ticket", Config: true,
			Positionals: []Positional{ticketArg}, Rest: "text", RestMin: 1, Run: runNote},
		{Name: "merge", Help: "merge a ticket's PR on the next tick", Config: true,
			Positionals: []Positional{ticketArg}, Run: runMerge},
		{Name: "ask", Help: "ask a question about a ticket; prints the child's id", Config: true,
			Positionals: []Positional{ticketArg}, Rest: "question", RestMin: 1, Run: runAsk},
		{Name: "reply", Help: "answer an agent's question", Config: true,
			Positionals: []Positional{ticketArg}, Rest: "text", RestMin: 1, Run: runReply},
		{Name: "answer", Help: "answer what a ticket asks", Config: true,
			Positionals: []Positional{ticketArg, {Name: "value"}},
			Flags:       []Flag{{Name: "note"}, {Name: "only"}, {Name: "kind"}, {Name: "since"}},
			Run:         runAnswer},
		{Name: "wake", Help: "start a tick now", Config: true, Run: runWake},
		{Name: "project", Help: "switch a factory project on or off over config's enabled, or follow config again", Config: true,
			Positionals: []Positional{{Name: "verb", Choices: []string{"enable", "disable", "follow"}}, {Name: "name"}},
			Run:         runProject},
		{Name: "migrate", Help: "bring old tickets up to date", Config: true, Run: runMigrate},
		{Name: "kinds", Help: "list the project kinds and what each factory project takes from its kind", Config: true,
			Flags: []Flag{{Name: "json", Kind: BoolFlag}}, Run: runKinds},
		{Name: "onboard", Help: "check whether a factory project is ready to switch on", Config: true,
			Positionals: []Positional{{Name: "project"}},
			Flags: []Flag{
				{Name: "checks", Kind: BoolFlag, Help: "also run the gate's checks on the base (waits for a running tick)"},
				{Name: "json", Kind: BoolFlag},
			},
			Run: runOnboard},
		{Name: "usage", Help: "the tokens the agents spent, largest first",
			Flags: []Flag{
				{Name: "days", Kind: IntFlag, Default: "7", Help: "the window: the last N days"},
				{Name: "by", Default: usage.ByProject, Choices: usage.Bys()},
				{Name: "json", Kind: BoolFlag},
			},
			Run: runUsage},
		{Name: "build", Help: "build a core commit on the builder helper and keep it as a candidate for deploy.sh", Config: true,
			Positionals: []Positional{{Name: "commit"}}, Flags: []Flag{{Name: "host", Help: "an ssh helper in [hosts], or self (default [build] host)"}}, Run: runBuild},
		{Name: "deploy", Help: "the master deploys core main with config main to itself (start, ask, run, hold, unhold, status, restore)", Config: true,
			Positionals: []Positional{{Name: "verb", Choices: deployVerbs()}, {Name: "rev", Optional: true, Help: "start: a commit on core main (default main); ask: the rev it shows"}},
			Flags: []Flag{
				{Name: "why", Help: "start, ask: why it deploys (for the log and the ask)"},
				{Name: "drill", Kind: BoolFlag, Help: "start: fail the check on purpose, to try the rollback (not on the running commit)"},
				{Name: "allow-older", Kind: BoolFlag, Help: "start: install even a commit that lacks the running one (a rollback)"},
				{Name: "note", Help: "hold: a note on the hold"},
				{Name: "force", Kind: BoolFlag, Help: "restore: put the snapshot back even when it is not of the last deploy"},
			},
			Run: runDeploy},
		// the tick
		{Name: "tick", Help: "run one tick of the pipeline", Config: true, Run: runTick},
		{Name: "early-checks", Help: "run Gate 1's checks of a ticket's head now and keep the result for its gate (the tick starts it)", Config: true,
			Positionals: []Positional{ticketArg, {Name: "head"}}, Run: runEarlyChecks},
		// the doors and upkeep
		{Name: "sweep", Help: "file tickets for open GitHub issues", Config: true, Run: runSweep},
		{Name: "clock", Help: "file the scheduled tickets that are due", Config: true, Run: runClock},
		{Name: "rebuild", Help: "rebuild the images", Config: true, Run: runRebuild,
			Flags: []Flag{
				{Name: "rollback", Metavar: "KIND", Help: "put the previous claude or pi image back and hold the version it had"},
				{Name: "unhold", Metavar: "KIND", Help: "let the next rebuild try a held version again"},
				{Name: "latest", Kind: BoolFlag, Help: "run the latest-toolchain trial now (never changes the live image)"},
			}},
		{Name: "versions", Help: "check the pinned versions", Config: true, Run: runVersions},
		// the screen relay: Foreman's Live screens while it is open
		{Name: "screens", Help: "relay the local agents' screens to Foreman while Live is open", Config: true, Run: runScreens},
		backupCommand(),
		// Fizzy boards into board projects
		fizzyCommand(),
		// the read-only commands and the ticket API
		{Name: "check", Help: "check the config (and with flags, the board)", Config: true,
			Flags: []Flag{
				{Name: "board", Kind: BoolFlag, Help: "list every ticket file that does not read cleanly"},
				{Name: "asks", Kind: BoolFlag, Help: "list the tickets in an ask column with no stored ask"},
			},
			Run: runCheck},
		{Name: "ticket", Help: "the ticket API (--json), or its human forms", Run: runTicket,
			Positionals: []Positional{{Name: "verb", Choices: ticketapi.Verbs()}}, Rest: "args",
			Flags: []Flag{
				{Name: "as", Metavar: "NAME", Help: "act as this token (default: the maintainer)"},
				{Name: "wait", Kind: FloatFlag, Default: "60", Help: "seconds to wait for a ticket's lock"},
				{Name: "json", Kind: BoolFlag, Help: "the request as one JSON object on stdin; the answer on stdout"},
				{Name: "project"},
			}},
		{Name: "tokens", Help: "add, list or revoke API tokens", Config: true, Run: runTokens,
			Positionals: []Positional{{Name: "verb", Choices: []string{"add", "list", "revoke"}}, {Name: "name", Optional: true}},
			Flags: []Flag{
				{Name: "project", Kind: RepeatFlag},
				{Name: "can", Help: "comma-separated: read,file,talk,work,plan"},
				{Name: "json", Kind: BoolFlag},
			}},
		{Name: "version", Help: "print the build's commit and Go version", Run: runVersion},
		// render
		{Name: "render", Help: "render the config into deployable files", Raw: ioRunner(runRender)},
		{Name: "patch-scion", Help: "patch Scion's provision.py", Raw: ioRunner(runPatchScion)},
	}
}
