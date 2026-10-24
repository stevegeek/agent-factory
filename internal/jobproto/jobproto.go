// Package jobproto is the dispatcher ↔ helper contract, shared by both
// sides: the nine SSH verbs, the job name, the start meta and its
// validation, the member names and caps of the start and collect tars,
// flat tars, the OpenSSH argv that reaches a helper's forced command, and
// how a failed verb reads.
//
// It holds data and pure functions only: sshjobs and jobtail run ssh, and
// the helper serves the verbs.
package jobproto

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stevegeek/agent-factory/internal/textx"
)

// The verbs a helper's forced command accepts over SSH. `run <job>`
// is only ever taken from the helper's own argv (systemd-run), never from
// SSH_ORIGINAL_COMMAND.
const (
	VerbStart   = "start"
	VerbStatus  = "status"
	VerbCollect = "collect"
	VerbTail    = "tail"
	VerbRemove  = "remove"
	VerbList    = "list"
	VerbVitals  = "vitals"  // the host's load, memory, disks, temperature and containers (JSON)
	VerbBuild   = "build"   // `build <commit12>`: a source tar in, the binaries out (a builder host only)
	VerbInstall = "install" // `install <commit12>`: the master's bundle in, installed here (an opted-in host only)
	VerbRun     = "run"     // argv only
)

// SSHVerbs is SSH_VERBS, in the helper's usage order.
func SSHVerbs() []string {
	return []string{VerbStart, VerbStatus, VerbCollect, VerbTail, VerbRemove, VerbList, VerbVitals, VerbBuild, VerbInstall}
}

// MaxBuildInput caps the source tar a build verb reads.
const MaxBuildInput = 32 << 20

// BuildFiles() are a build's answer: build.sh's three binaries and its BUILD
// file, in the order BUILD lists them.
func BuildFiles() []string { return []string{"factory", "foreman", "factory-remote", "BUILD"} }

// MaxInstallInput caps the bundle an install verb reads.
const MaxInstallInput = 64 << 20

// InstallFiles are the files an install bundle must hold, as paths under
// the factory root: what deploy.sh's helper-bundle sends a helper.
func InstallFiles() []string {
	return []string{"bin/factory-remote", "bin/BUILD", "bin/pi-run", "config.toml"}
}

// InstallDirs are the trees an install bundle may hold, under the factory
// root: the units and the image contexts.
func InstallDirs() []string { return []string{"systemd", "images/pi", "images/scion-agent"} }

// AcceptInstalls is the file in a helper's factory root that opts it in to
// installs from the master. Only the maintainer creates it; nothing the
// helper runs writes it.
const AcceptInstalls = "accept-installs"

// re: go
var commit12 = regexp.MustCompile(`^[0-9a-f]{12}\z`)

// Commit12OK reports a commit named as deploy.sh and BUILD name it: the
// first 12 hex digits of its id.
func Commit12OK(s string) bool { return commit12.MatchString(s) }

// ToolchainPin is the toolchain line of a go.mod ("go1.27.1"): the Go a
// build of that tree is pinned to.
func ToolchainPin(gomod []byte) (string, error) {
	for line := range strings.Lines(string(gomod)) {
		line, _, _ = strings.Cut(line, "//")
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "toolchain" && strings.HasPrefix(f[1], "go") && len(f[1]) > 2 && !strings.ContainsAny(f[1], "/\\") {
			return f[1], nil
		}
	}
	return "", errors.New("go.mod has no toolchain line")
}

// IsSSHVerb reports a verb a dispatcher may send.
func IsSSHVerb(verb string) bool { return slices.Contains(SSHVerbs(), verb) }

// Job kinds: an agent's run, or Gate 1's checks (install, tests, lint) on
// a fresh checkout of one commit. Only a pull host takes gate jobs; a
// helper's factory-remote refuses them.
const (
	KindAgent = "agent"
	KindGate  = "gate"
)

// The states `status` answers with ({"state": ...}).
const (
	StateRunning = "running"
	StateDone    = "done"
	StateMissing = "missing"
)

// Limits of the contract (bin/factory-remote).
const (
	DefaultModel          = "claude-sonnet-5-5"
	DefaultHarness        = "pi"
	DefaultTimeoutMinutes = 45
	MaxTimeoutMinutes     = 120
	MaxInputBytes         = 200 << 20 // a start tar
	MinFreeBytes          = 2 << 30   // free disk a start needs
	TailBytes             = 64 << 10  // what `tail` sends of agent.log
	// RuntimeSlack is added to a job's timeout for its systemd unit's
	// RuntimeMaxSec (timeout·60 + 300).
	RuntimeSlack = 300 * time.Second
)

// Member names of the start tar. Memory and pinned files
// sit under MemoryDir and UntrackedDir.
const (
	MemberPrompt    = "prompt.md"
	MemberRole      = "role.md"
	MemberMeta      = "meta.json"
	MemberBundle    = "repo.bundle"
	MemoryDir       = "memory/"
	UntrackedDir    = "untracked/"
	MemberReport    = "report.json"
	MemberHead      = "head"
	MemberLog       = "agent.log"
	MemberOutBundle = "out.bundle"
	MemberMemoryTar = "memory.tar"
	// MemberUsage is a job's token usage, which the helper reads from the
	// agent's home after it exits (usage.Usage's JSON).
	MemberUsage = "usage.json"
	// MemberChecks is a gate job's commands: the project's
	// "set -e; <install>; <test>; <lint>", run in the checkout.
	MemberChecks = "checks.sh"
)

// Cap is one collect member and its size cap.
type Cap struct {
	Name string
	Max  int64
}

// CollectCaps is MAX_COLLECTED_BYTES, in the order collect writes the
// members. A member over its cap is left out, except agent.log, which is
// cut to its tail.
func CollectCaps() []Cap {
	return []Cap{
		{MemberReport, 1_000_000},
		{MemberPrompt, 1_000_000},
		{MemberHead, 1_000},
		{MemberLog, 2_000_000},
		{MemberOutBundle, 100 << 20},
		{MemberMemoryTar, 4_000_000},
		{MemberUsage, 64 << 10},
	}
}

// Timeout is the dispatcher's limit on one verb:
// collect 300 s, tail and vitals 20 s, everything else 120 s.
func Timeout(verb string) time.Duration {
	switch verb {
	case VerbCollect:
		return 300 * time.Second
	case VerbBuild:
		return 20 * time.Minute
	case VerbInstall:
		return 5 * time.Minute
	case VerbTail, VerbVitals:
		return 20 * time.Second
	}
	return 120 * time.Second
}

// OutputCap bounds what the dispatcher reads back from one verb: the
// collect tar (its members' caps plus headers), MaxVitals for vitals, or
// 1 MiB for the small answers. Python reads without a bound; a helper
// sending more is an error.
func OutputCap(verb string) int {
	switch verb {
	case VerbCollect:
		return 128 << 20
	case VerbBuild:
		return 96 << 20
	case VerbVitals:
		return MaxVitals
	}
	return 1 << 20
}

// MaxVitals caps a host's vitals JSON: the vitals verb's answer, a pull
// host's vitals post.
const MaxVitals = 64 << 10

// py: ^[a-z0-9][a-z0-9-]{0,40}$
var jobName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}\z`)

// JobNameOK is JOB_NAME: ^[a-z0-9][a-z0-9-]{0,40}$. Unlike Python's `$`,
// a trailing newline is refused (as memstore.KeyOK does): the factory never
// makes such a name, and a job name ends up in paths and unit names.
func JobNameOK(name string) bool { return jobName.MatchString(name) }

// Host is how the dispatcher reaches one helper host ([hosts.<name>]).
type Host struct {
	Name string // the [hosts.<name>] key: what tickets and pauses name
	SSH  string // the ssh destination, e.g. factory@helper
	Key  string // the private key's path, already expanded (no ~)
}

// SSHArgv is the argv of one verb on a helper (remote()): OpenSSH
// with BatchMode, a 10 s connect timeout and only the given key, then the
// verb and its job, which the helper's forced command reads from
// SSH_ORIGINAL_COMMAND. The verb must be one of SSHVerbs; every verb but
// list and vitals takes exactly one job name. Both sshjobs and jobtail use
// it.
func SSHArgv(h Host, verb, job string) ([]string, error) {
	if h.SSH == "" || h.SSH[0] == '-' {
		return nil, fmt.Errorf("helper host %q: bad ssh destination %q", h.Name, h.SSH)
	}
	if h.Key == "" {
		return nil, fmt.Errorf("helper host %q: no ssh key", h.Name)
	}
	if !IsSSHVerb(verb) {
		return nil, fmt.Errorf("helper host %q: %q is not a helper verb", h.Name, verb)
	}
	argv := []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "IdentitiesOnly=yes",
		"-i", h.Key, h.SSH, verb}
	if verb == VerbList || verb == VerbVitals {
		if job != "" {
			return nil, fmt.Errorf("helper host %q: %s takes no job", h.Name, verb)
		}
		return argv, nil
	}
	if !JobNameOK(job) {
		return nil, fmt.Errorf("helper host %q: bad job name %q", h.Name, job)
	}
	return append(argv, job), nil
}

// RemoteError is a verb that failed on its helper or did not answer in
// time (Python RemoteError): the tick takes it as "try another host".
type RemoteError struct {
	SSH, Verb string
	Msg       string // the whole message, as Python's str(e)
}

func (e *RemoteError) Error() string { return e.Msg }

// Failed is the RemoteError of a verb that exited non-zero:
// "<ssh> <verb> failed (<rc>): <last 300 chars of stderr, stripped>".
func Failed(ssh, verb string, code int, stderr []byte) *RemoteError {
	tail := lastRunes(textx.Strip(DecodeText(stderr)), 300)
	return &RemoteError{SSH: ssh, Verb: verb, Msg: fmt.Sprintf("%s %s failed (%d): %s", ssh, verb, code, tail)}
}

// TimedOut is the RemoteError of a verb past its timeout.
func TimedOut(ssh, verb string) *RemoteError {
	return &RemoteError{SSH: ssh, Verb: verb, Msg: fmt.Sprintf("%s %s timed out", ssh, verb)}
}

// TooMuch is the RemoteError of a verb that sent more than OutputCap.
func TooMuch(ssh, verb string) *RemoteError {
	return &RemoteError{SSH: ssh, Verb: verb, Msg: fmt.Sprintf("%s %s sent more than %d bytes", ssh, verb, OutputCap(verb))}
}

// ErrStatus is a status answer that is not {"state": ...}.
var ErrStatus = errors.New("status: not a job state")

func lastRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := len(s)
	for range n {
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
	}
	return s[i:]
}

// DecodeText is bytes.decode("utf-8", errors="replace"): each maximal
// invalid subpart becomes one U+FFFD, as CPython's decoder does (Go's
// conversions replace byte by byte, or whole runs).
func DecodeText(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	out := make([]byte, 0, len(b)+8)
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r != utf8.RuneError || size > 1 {
			out = append(out, b[i:i+size]...)
			i += size
			continue
		}
		i += invalidSpan(b[i:])
		out = utf8.AppendRune(out, utf8.RuneError)
	}
	return string(out)
}

// invalidSpan is the length of the maximal subpart at the start of b, an
// invalid sequence: the lead byte and the continuation bytes that would
// still have fitted a well-formed sequence.
func invalidSpan(b []byte) int {
	lead := b[0]
	var need int
	lo, hi := byte(0x80), byte(0xBF)
	switch {
	case lead >= 0xC2 && lead <= 0xDF:
		need = 1
	case lead == 0xE0:
		need, lo = 2, 0xA0
	case lead >= 0xE1 && lead <= 0xEC, lead == 0xEE, lead == 0xEF:
		need = 2
	case lead == 0xED:
		need, hi = 2, 0x9F
	case lead == 0xF0:
		need, lo = 3, 0x90
	case lead >= 0xF1 && lead <= 0xF3:
		need = 3
	case lead == 0xF4:
		need, hi = 3, 0x8F
	default:
		return 1
	}
	n := 1
	for k := 0; k < need && n < len(b); k++ {
		c := b[n]
		if k == 0 && (c < lo || c > hi) || k > 0 && (c < 0x80 || c > 0xBF) {
			break
		}
		n++
	}
	return n
}
