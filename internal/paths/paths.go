// Package paths names every file and directory the factory uses. FromEnv
// is the only place in the module that reads FACTORY_ROOT and HOME, but
// for helper.Main (factory-remote does not link this package); every other
// package is handed the Paths value it needs, so nothing reads the
// environment or a global.
package paths

import (
	"os/user"
	"path/filepath"
)

// Paths is the factory's layout under one root and one home. The values are
// those of the Python constants in bin/factory (ROOT, BOARD, LOGS, ...).
type Paths struct {
	Root string // $FACTORY_ROOT or ~/factory
	Bin  string // Root/bin: the only place wake finds the factory binary

	Board      string // Root/board
	Tickets    string // Root/board/tickets (Python BOARD)
	Locks      string // Root/board/tickets/.locks: one flock file per ticket
	Index      string // Root/board/index.json
	IDLock     string // Root/board/tickets/.id.lock
	LastID     string // Root/board/tickets/.last-id
	IndexLock  string // Root/board/.index.lock
	CreateLock string // Root/board/.create.lock

	Logs       string // Root/logs
	FactoryLog string // Root/logs/factory.log
	LogLock    string // Root/logs/.log.lock: taken only while rotating

	Gate    string // Root/gate
	Memory  string // Root/memory: per (project, role) bare git stores
	Context string // Root/context: per project, the maintainer's notes
	Run     string // Root/run
	Manager string // Root/manager
	Images  string // Root/images

	Config    string // Root/config.toml
	State     string // Root/state.json
	StateLock string // Root/.state.lock: held while state.json is read, changed and written
	Watches   string // Root/watches.json
	Pauses    string // Root/pauses.json
	TickLock  string // Root/.tick.lock
	TickAgain string // Root/.tick.again

	Home           string // $HOME, or the account's home directory
	OAuthToken     string // ~/.config/factory/claude-oauth-token
	AppKey         string // ~/.config/factory/github-app.pem
	Tokens         string // $FACTORY_API_TOKENS or ~/.config/factory/api-tokens.json
	Scion          string // ~/.local/bin/scion
	ScionTemplates string // ~/.scion/templates
	AgentHomes     string // glob pattern; {name} stands for the agent's name
	RemoteKey      string // ~/.ssh/factory-remote
}

// FromEnv builds the layout from an environment. As in Python, an empty
// FACTORY_ROOT or FACTORY_API_TOKENS counts as unset, and an unset HOME
// falls back to the account's home directory (os.path.expanduser).
func FromEnv(env map[string]string) Paths {
	home := env["HOME"]
	if home == "" {
		if u, err := user.Current(); err == nil {
			home = u.HomeDir
		}
	}
	root := env["FACTORY_ROOT"]
	if root == "" {
		root = filepath.Join(home, "factory")
	}
	tokens := env["FACTORY_API_TOKENS"]
	if tokens == "" {
		tokens = filepath.Join(home, ".config", "factory", "api-tokens.json")
	}
	board := filepath.Join(root, "board")
	tickets := filepath.Join(board, "tickets")
	logs := filepath.Join(root, "logs")
	conf := filepath.Join(home, ".config", "factory")
	return Paths{
		Root: root,
		Bin:  filepath.Join(root, "bin"),

		Board:      board,
		Tickets:    tickets,
		Locks:      filepath.Join(tickets, ".locks"),
		Index:      filepath.Join(board, "index.json"),
		IDLock:     filepath.Join(tickets, ".id.lock"),
		LastID:     filepath.Join(tickets, ".last-id"),
		IndexLock:  filepath.Join(board, ".index.lock"),
		CreateLock: filepath.Join(board, ".create.lock"),

		Logs:       logs,
		FactoryLog: filepath.Join(logs, "factory.log"),
		LogLock:    filepath.Join(logs, ".log.lock"),

		Gate:    filepath.Join(root, "gate"),
		Memory:  filepath.Join(root, "memory"),
		Context: filepath.Join(root, "context"),
		Run:     filepath.Join(root, "run"),
		Manager: filepath.Join(root, "manager"),
		Images:  filepath.Join(root, "images"),

		Config:    filepath.Join(root, "config.toml"),
		State:     filepath.Join(root, "state.json"),
		StateLock: filepath.Join(root, ".state.lock"),
		Watches:   filepath.Join(root, "watches.json"),
		Pauses:    filepath.Join(root, "pauses.json"),
		TickLock:  filepath.Join(root, ".tick.lock"),
		TickAgain: filepath.Join(root, ".tick.again"),

		Home:           home,
		OAuthToken:     filepath.Join(conf, "claude-oauth-token"),
		AppKey:         filepath.Join(conf, "github-app.pem"),
		Tokens:         tokens,
		Scion:          filepath.Join(home, ".local", "bin", "scion"),
		ScionTemplates: filepath.Join(home, ".scion", "templates"),
		AgentHomes:     filepath.Join(home, ".scion", "project-configs", "*", ".scion", "agents", "{name}", "home"),
		RemoteKey:      filepath.Join(home, ".ssh", "factory-remote"),
	}
}

// TicketFile is board/tickets/<id>.json. The id is used as given: callers
// validate and upper-case it first.
func (p Paths) TicketFile(id string) string { return filepath.Join(p.Tickets, id+".json") }

// TicketLock is the ticket's flock file, board/tickets/.locks/<id>.
func (p Paths) TicketLock(id string) string { return filepath.Join(p.Locks, id) }

// TicketLogs is logs/<id>, where a ticket's runs are archived.
func (p Paths) TicketLogs(id string) string { return filepath.Join(p.Logs, id) }

// MemoryRepo is memory/<project>/<role>.git.
func (p Paths) MemoryRepo(project, role string) string {
	return filepath.Join(p.Memory, project, role+".git")
}

// ProjectContext is context/<project>.
func (p Paths) ProjectContext(project string) string { return filepath.Join(p.Context, project) }

// FactoryBin is bin/factory under the root: what wake runs, never
// os.Executable() (inside Foreman that is foreman).
func (p Paths) FactoryBin() string { return filepath.Join(p.Bin, "factory") }

// ForemanState is Root/foreman-state, Foreman's own writable state.
func (p Paths) ForemanState() string { return filepath.Join(p.Root, "foreman-state") }

// ForemanLog is Root/foreman-state/foreman.log, Foreman's own log: logs/
// is read-only to it. The Live view shows it with factory.log.
func (p Paths) ForemanLog() string { return filepath.Join(p.ForemanState(), "foreman.log") }

// ProjectSwitches is Root/foreman-state/projects.json: the maintainer's
// switches that turn a factory project on or off over config.toml's
// `enabled` (config.Switches). Foreman writes it, as `factory project`
// does; every reader of the config applies it.
func (p Paths) ProjectSwitches() string { return filepath.Join(p.ForemanState(), "projects.json") }

// WakeTrigger is Root/foreman-state/wake: Foreman writes it to wake the
// tick, and the factory-wake.path user unit starts one.
func (p Paths) WakeTrigger() string { return filepath.Join(p.ForemanState(), "wake") }

// Watching is Root/foreman-state/watching: Foreman touches it while Live is
// open, and the factory-screens.path user unit starts the screen relay
// (package screens), which reads only its mtime.
func (p Paths) Watching() string { return filepath.Join(p.ForemanState(), "watching") }

// Screens is Root/screens: the relay writes each running local agent's
// screen there, and Foreman, which cannot run Scion, reads them.
func (p Paths) Screens() string { return filepath.Join(p.Root, "screens") }

// Builds is Root/builds: build candidates, one dir per commit (package
// builds), which deploy.sh installs from.
func (p Paths) Builds() string { return filepath.Join(p.Root, "builds") }

// Src is Root/src/<commit12>: the source tree deploy.sh extracts there for
// that commit, which `factory build` sends to the builder.
func (p Paths) Src(commit string) string { return filepath.Join(p.Root, "src", commit) }

// Deploys is Root/deploys: the self-deploy's request, state, snapshot,
// work dir, logs and repo mirrors (package deploy).
func (p Paths) Deploys() string { return filepath.Join(p.Root, "deploys") }

// DeployRequest is Root/deploys/request.json: the deploy waiting for the
// runner (one; a newer request replaces it).
func (p Paths) DeployRequest() string { return filepath.Join(p.Deploys(), "request.json") }

// DeployState is Root/deploys/state.json: the running or last deploy.
func (p Paths) DeployState() string { return filepath.Join(p.Deploys(), "state.json") }

// DeployLock is Root/deploys/.lock, held by the runner for its whole run;
// deploy.sh prepare refuses while it is held.
func (p Paths) DeployLock() string { return filepath.Join(p.Deploys(), ".lock") }

// DeployBefore is Root/deploys/before: the snapshot of the running files a
// failed deploy restores.
func (p Paths) DeployBefore() string { return filepath.Join(p.Deploys(), "before") }

// DeployWork is Root/deploys/work: one run's deploy.sh, config and staging
// directory, cleared at the start of each run.
func (p Paths) DeployWork() string { return filepath.Join(p.Deploys(), "work") }

// DeployLogs is Root/deploys/logs: each run's deploy.sh output.
func (p Paths) DeployLogs() string { return filepath.Join(p.Deploys(), "logs") }

// DeployConfigCommit is Root/deploys/config-commit: the config commit the
// running factory was deployed with, written by a good self-deploy and by
// deploy.sh install from a config checkout.
func (p Paths) DeployConfigCommit() string { return filepath.Join(p.Deploys(), "config-commit") }

// CoreMirror is Root/deploys/core.git, the bare mirror of core's main.
func (p Paths) CoreMirror() string { return filepath.Join(p.Deploys(), "core.git") }

// ConfigMirror is Root/deploys/config.git, the bare mirror of config's main.
func (p Paths) ConfigMirror() string { return filepath.Join(p.Deploys(), "config.git") }

// OperatorWork is Root/operator/work: the temp clones the operator lands
// a manager's config draft in, one per draft, removed on every exit.
func (p Paths) OperatorWork() string { return filepath.Join(p.Root, "operator", "work") }

// DeployHold is Root/foreman-state/deploy-hold.json: present while the
// maintainer holds automatic deploys (they become Deploy? asks). Foreman
// writes it, as `factory deploy hold` does.
func (p Paths) DeployHold() string { return filepath.Join(p.ForemanState(), "deploy-hold.json") }

// AcceptInstalls is Root/accept-installs (jobproto.AcceptInstalls): a
// helper's opt-in to factory-remote install, which only the owner creates.
func (p Paths) AcceptInstalls() string { return filepath.Join(p.Root, "accept-installs") }

// UsageLog is Root/usage.jsonl: one line per finished agent run, what it
// spent in tokens (internal/usage).
func (p Paths) UsageLog() string { return filepath.Join(p.Root, "usage.jsonl") }

// ImagesState is Root/images.json, the agent versions the weekly rebuild
// installed. A deploy never writes it.
func (p Paths) ImagesState() string { return filepath.Join(p.Root, "images.json") }

// Mailbox is Root/mailbox: the jobs queued for pull hosts, one directory
// per host (internal/mailbox).
func (p Paths) Mailbox() string { return filepath.Join(p.Root, "mailbox") }

// SecretsDir is ~/.config/factory, where the credentials live.
func (p Paths) SecretsDir() string { return filepath.Join(p.Home, ".config", "factory") }

// AnthropicKey is ~/.config/factory/anthropic-api-key (pi-run's key).
func (p Paths) AnthropicKey() string { return filepath.Join(p.SecretsDir(), "anthropic-api-key") }

// BackupKey is ~/.config/factory/backup-key, the backups' AES-256 key.
func (p Paths) BackupKey() string { return filepath.Join(p.SecretsDir(), "backup-key") }

// BackupCredentials is ~/.config/factory/r2-credentials, the default file
// holding the R2 access key pair.
func (p Paths) BackupCredentials() string { return filepath.Join(p.SecretsDir(), "r2-credentials") }

// Sealed are the credential files Foreman's process must never be able to
// open: the unit's InaccessiblePaths and /healthz's check.
func (p Paths) Sealed() []string {
	return []string{p.AppKey, p.OAuthToken, p.AnthropicKey(), p.BackupKey(), p.BackupCredentials()}
}
