package paths

import (
	"path/filepath"
	"reflect"
	"testing"
)

// fields lists every string field of Paths by name.
func (p Paths) fields() map[string]string {
	out := map[string]string{}
	v := reflect.ValueOf(p)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).Kind() == reflect.String {
			out[v.Type().Field(i).Name] = v.Field(i).String()
		}
	}
	return out
}

func TestFromEnvLayout(t *testing.T) {
	p := FromEnv(map[string]string{"FACTORY_ROOT": "/srv/f", "HOME": "/h"})
	want := map[string]string{
		"Root": "/srv/f", "Bin": "/srv/f/bin", "Board": "/srv/f/board", "Tickets": "/srv/f/board/tickets",
		"Locks": "/srv/f/board/tickets/.locks", "Index": "/srv/f/board/index.json",
		"IDLock": "/srv/f/board/tickets/.id.lock", "LastID": "/srv/f/board/tickets/.last-id",
		"IndexLock": "/srv/f/board/.index.lock", "CreateLock": "/srv/f/board/.create.lock",
		"Logs": "/srv/f/logs", "FactoryLog": "/srv/f/logs/factory.log", "LogLock": "/srv/f/logs/.log.lock",
		"Gate": "/srv/f/gate", "Memory": "/srv/f/memory", "Context": "/srv/f/context", "Run": "/srv/f/run",
		"Manager": "/srv/f/manager", "Images": "/srv/f/images",
		"Config": "/srv/f/config.toml", "State": "/srv/f/state.json", "StateLock": "/srv/f/.state.lock", "Watches": "/srv/f/watches.json",
		"Pauses": "/srv/f/pauses.json", "TickLock": "/srv/f/.tick.lock", "TickAgain": "/srv/f/.tick.again",
		"Home": "/h", "OAuthToken": "/h/.config/factory/claude-oauth-token", "AppKey": "/h/.config/factory/github-app.pem",
		"Tokens": "/h/.config/factory/api-tokens.json", "Scion": "/h/.local/bin/scion",
		"ScionTemplates": "/h/.scion/templates", "AgentHomes": "/h/.scion/project-configs/*/.scion/agents/{name}/home",
		"RemoteKey": "/h/.ssh/factory-remote",
	}
	got := p.fields()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("field %s is not covered by the test", k)
		}
	}
}

func TestDerivedPaths(t *testing.T) {
	p := FromEnv(map[string]string{"FACTORY_ROOT": "/srv/f", "HOME": "/h"})
	for got, want := range map[string]string{
		p.ForemanState():       "/srv/f/foreman-state",
		p.WakeTrigger():        "/srv/f/foreman-state/wake",
		p.Watching():           "/srv/f/foreman-state/watching",
		p.Screens():            "/srv/f/screens",
		p.ImagesState():        "/srv/f/images.json",
		p.UsageLog():           "/srv/f/usage.jsonl",
		p.Mailbox():            "/srv/f/mailbox",
		p.EarlyChecks():        "/srv/f/gate-early",
		p.Deploys():            "/srv/f/deploys",
		p.DeployRequest():      "/srv/f/deploys/request.json",
		p.DeployState():        "/srv/f/deploys/state.json",
		p.DeployLock():         "/srv/f/deploys/.lock",
		p.DeployBefore():       "/srv/f/deploys/before",
		p.DeployWork():         "/srv/f/deploys/work",
		p.DeployLogs():         "/srv/f/deploys/logs",
		p.DeployConfigCommit(): "/srv/f/deploys/config-commit",
		p.CoreMirror():         "/srv/f/deploys/core.git",
		p.ConfigMirror():       "/srv/f/deploys/config.git",
		p.DeployHold():         "/srv/f/foreman-state/deploy-hold.json",
		p.OperatorWork():       "/srv/f/operator/work",
		p.AcceptInstalls():     "/srv/f/accept-installs",
		p.SecretsDir():         "/h/.config/factory",
		p.AnthropicKey():       "/h/.config/factory/anthropic-api-key",
		p.BackupKey():          "/h/.config/factory/backup-key",
		p.BackupCredentials():  "/h/.config/factory/r2-credentials",
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	want := []string{"/h/.config/factory/github-app.pem", "/h/.config/factory/claude-oauth-token",
		"/h/.config/factory/anthropic-api-key", "/h/.config/factory/backup-key", "/h/.config/factory/r2-credentials"}
	if !reflect.DeepEqual(p.Sealed(), want) {
		t.Errorf("Sealed() = %q, want %q", p.Sealed(), want)
	}
}

func TestFromEnvDefaultsAndOverrides(t *testing.T) {
	cases := []struct {
		name            string
		env             map[string]string
		root, home, tok string
	}{
		{"root defaults to ~/factory", map[string]string{"HOME": "/h"}, "/h/factory", "/h", "/h/.config/factory/api-tokens.json"},
		{"an empty FACTORY_ROOT counts as unset (Python `or`)", map[string]string{"HOME": "/h", "FACTORY_ROOT": ""}, "/h/factory", "/h", "/h/.config/factory/api-tokens.json"},
		{"FACTORY_API_TOKENS overrides the token store", map[string]string{"HOME": "/h", "FACTORY_API_TOKENS": "/t/tok.json"}, "/h/factory", "/h", "/t/tok.json"},
		{"an empty FACTORY_API_TOKENS counts as unset", map[string]string{"HOME": "/h", "FACTORY_API_TOKENS": ""}, "/h/factory", "/h", "/h/.config/factory/api-tokens.json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := FromEnv(c.env)
			if p.Root != c.root || p.Home != c.home || p.Tokens != c.tok {
				t.Errorf("got root %q home %q tokens %q", p.Root, p.Home, p.Tokens)
			}
		})
	}
}

func TestHomeFallsBackToTheAccountWhenUnset(t *testing.T) {
	p := FromEnv(map[string]string{})
	if p.Home == "" || !filepath.IsAbs(p.Home) {
		t.Fatalf("home %q: want the account's home directory", p.Home)
	}
	if p.Root != filepath.Join(p.Home, "factory") {
		t.Errorf("root %q", p.Root)
	}
}

func TestPerTicketAndStorePaths(t *testing.T) {
	p := FromEnv(map[string]string{"FACTORY_ROOT": "/r", "HOME": "/h"})
	for _, c := range [][2]string{
		{p.TicketFile("T0042"), "/r/board/tickets/T0042.json"},
		{p.TicketLock("T0042"), "/r/board/tickets/.locks/T0042"},
		{p.TicketLogs("T0042"), "/r/logs/T0042"},
		{p.MemoryRepo("gem", "worker"), "/r/memory/gem/worker.git"},
		{p.ProjectContext("gem"), "/r/context/gem"},
		{p.FactoryBin(), "/r/bin/factory"},
	} {
		if c[0] != c[1] {
			t.Errorf("got %q, want %q", c[0], c[1])
		}
	}
}
