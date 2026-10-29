package agentfactory

// The root scripts (systemd/install-units.sh, host/allow-puller.sh) run
// here as root in a user namespace of their own, with a tmpfs over the
// directories they write (/etc/systemd/system, /etc/ssh) and fakes for
// systemctl, sshd, getent and id on PATH. Nothing on this machine changes; the
// test skips where unprivileged user namespaces are not allowed.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeSystemctl keeps each unit's state under $FAKE: active/<unit> for a
// started unit; enable links timers.target.wants/<unit>. Like systemctl, a
// disable or enable naming a unit file that is not there does nothing at
// all.
const fakeSystemctl = `#!/bin/bash
echo "systemctl $*" >>"$FAKE/log"
d=/etc/systemd/system w=/etc/systemd/system/timers.target.wants a=$FAKE/active
mkdir -p "$a"
cmd=$1; shift
now=
if [[ ${1-} == --now ]]; then now=1; shift; fi
case $cmd in
  daemon-reload) ;;
  stop) for u; do rm -f "$a/$u"; done ;;
  disable|enable)
    for u; do [[ -f $d/$u ]] || { echo "Unit file $u does not exist." >&2; exit 1; }; done
    mkdir -p "$w"
    for u; do
      if [[ $cmd == enable ]]; then ln -sf "$d/$u" "$w/$u"; else rm -f "$w/$u"; fi
      if [[ -n $now && $cmd == enable ]]; then touch "$a/$u"; fi
      if [[ -n $now && $cmd == disable ]]; then rm -f "$a/$u"; fi
    done ;;
  list-timers) ls "$a" ;;
  *) echo "fake systemctl: $cmd" >&2; exit 1 ;;
esac
`

// asRoot runs body under bash as root in a new user and mount namespace,
// after a tmpfs is mounted over each of dirs. FAKE is a scratch directory
// whose bin/ (with the fakes) comes first on PATH. It returns the combined
// output and the exit code. args are body's $1, $2...
func asRoot(t *testing.T, fakes map[string]string, dirs []string, body string, args ...string) (string, int) {
	t.Helper()
	need(t, "unshare", "bash", "mount")
	if err := exec.Command("unshare", "-rm", "true").Run(); err != nil {
		t.Skipf("no unprivileged user namespaces: %v", err)
	}
	for _, d := range dirs {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Skipf("%s is not a directory here", d)
		}
	}
	fake := t.TempDir()
	for name, src := range fakes {
		mkexec(t, filepath.Join(fake, "bin", name), []byte(src))
	}
	var pre strings.Builder
	pre.WriteString("set -u\n")
	for _, d := range dirs {
		pre.WriteString("mount -t tmpfs none '" + d + "' || exit 99\n")
	}
	cmd := exec.Command("unshare", append([]string{"-rm", "bash", "-c", pre.String() + body, "bash"}, args...)...)
	cmd.Env = append(os.Environ(), "FAKE="+fake, "PATH="+filepath.Join(fake, "bin")+":"+os.Getenv("PATH"))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	if code == 99 {
		t.Skipf("cannot mount a tmpfs here:\n%s", out.String())
	}
	return out.String(), code
}

// unitsDir is systemd/ copied as host/setup.sh copies it: the templates
// and the installer, owned by this namespace's root and writable by no one
// else.
func unitsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files, err := filepath.Glob("systemd/*")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		copyFile(t, f, filepath.Join(dir, filepath.Base(f)))
	}
	if err := os.Chmod(filepath.Join(dir, "install-units.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// unitFakes are what install-units.sh meets besides systemctl: a factory
// user with uid 1001 whose home is $FAKE/home, and a root user.
var unitFakes = map[string]string{
	"systemctl": fakeSystemctl,
	"id": `#!/bin/bash
[[ $1 == -u ]] || exit 1
case $2 in factory) echo 1001 ;; root) echo 0 ;; *) exit 1 ;; esac
`,
	"getent": `#!/bin/bash
[[ $1 == passwd ]] || exit 2
case $2 in
  factory) echo "factory:x:1001:1001::$FAKE/home:/bin/bash" ;;
  root) echo "root:x:0:0::/root:/bin/bash" ;;
  *) exit 2 ;;
esac
`,
}

// deployedUnits is the body's prelude: the factory user's own rendered
// copy of the rebuild timer, as a deploy leaves it.
const deployedUnits = `
mkdir -p "$FAKE/home/factory/systemd"
printf '[Timer]\nOnCalendar=Mon *-*-* 03:15:00\n' >"$FAKE/home/factory/systemd/factory-rebuild.timer"
`

// unitState lists the factory units in /etc/systemd/system, the wants
// links and the active units, each line prefixed by where it is.
const unitState = `
for f in /etc/systemd/system/*; do [[ -f $f ]] && echo "unit ${f##*/}"; done
for f in /etc/systemd/system/timers.target.wants/*; do [[ -L $f ]] && echo "wants ${f##*/}"; done
for f in "$FAKE"/active/*; do [[ -e $f ]] && echo "active ${f##*/}"; done
true
`

func stateLines(out string) []string {
	var got []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "unit ") || strings.HasPrefix(l, "wants ") || strings.HasPrefix(l, "active ") {
			got = append(got, l)
		}
	}
	return got
}

// A helper installed before the clock timer existed still has its tick
// timer armed: --helper stops and disables each timer on its own (a single
// disable naming the missing clock timer would stop none), removes the
// wants links and the unit files. A second run changes nothing.
func TestInstallUnitsHelperDisarmsAnOlderDispatcher(t *testing.T) {
	src := unitsDir(t)
	out, code := asRoot(t, unitFakes, []string{"/etc/systemd/system"}, deployedUnits+`
src=$1
d=/etc/systemd/system w=$d/timers.target.wants
mkdir -p "$w" "$FAKE/active"
for u in factory@.service factory-tick.timer factory-sweep.timer; do cp "$src/$u" "$d/"; done
for u in factory-tick.timer factory-sweep.timer; do ln -s "$d/$u" "$w/$u"; touch "$FAKE/active/$u"; done
bash "$src/install-units.sh" --helper --enable >/dev/null || { echo "first run failed"; exit 1; }
`+unitState+`
echo ---
bash "$src/install-units.sh" --helper --enable >/dev/null || { echo "second run failed"; exit 1; }
`+unitState, src)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	first, second, _ := strings.Cut(out, "---\n")
	want := []string{"unit factory-rebuild.service", "unit factory-rebuild.timer", "wants factory-rebuild.timer",
		"active factory-rebuild.timer"}
	if got := stateLines(first); !slices.Equal(got, want) {
		t.Errorf("after --helper:\n%s\nwant %q", strings.Join(got, "\n"), want)
	}
	if got := stateLines(second); !slices.Equal(got, want) {
		t.Errorf("after a second --helper:\n%s\nwant %q", strings.Join(got, "\n"), want)
	}
}

// Without --helper, --enable installs and arms every dispatcher timer.
func TestInstallUnitsDispatcherArmsItsTimers(t *testing.T) {
	src := unitsDir(t)
	out, code := asRoot(t, unitFakes, []string{"/etc/systemd/system"}, deployedUnits+`
bash "$1/install-units.sh" --enable >/dev/null || { echo "run failed"; exit 1; }
`+unitState, src)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	got := stateLines(out)
	for _, u := range []string{"factory-tick.timer", "factory-sweep.timer", "factory-clock.timer", "factory-rebuild.timer"} {
		for _, l := range []string{"unit " + u, "wants " + u, "active " + u} {
			if !slices.Contains(got, l) {
				t.Errorf("no %q in:\n%s", l, strings.Join(got, "\n"))
			}
		}
	}
	if !slices.Contains(got, "unit factory@.service") {
		t.Errorf("factory@.service not installed:\n%s", strings.Join(got, "\n"))
	}
}

// A helper opts in to the watchdog with --watchdog: --enable arms its
// timer next to the rebuild's. A later --helper run without it disarms and
// removes the watchdog (as does a dispatcher run), and --watchdog without
// --helper is refused before anything is installed.
// A routine re-run without --watchdog says it removed the watch; a refused run
// leaves an armed watchdog alone; arming twice changes nothing; a bundle that
// lacks a unit file stops before anything is installed.
func TestInstallUnitsWatchdogEdges(t *testing.T) {
	src := unitsDir(t)
	out, code := asRoot(t, unitFakes, []string{"/etc/systemd/system"}, deployedUnits+`
src=$1
bash "$src/install-units.sh" --helper --watchdog --enable >/dev/null || { echo "opt-in failed"; exit 1; }
bash "$src/install-units.sh" --helper --watchdog --enable >/dev/null || { echo "re-arm failed"; exit 1; }
`+unitState+`
echo ---
bash "$src/install-units.sh" --watchdog --enable >/dev/null 2>&1; echo "refused $?"
`+unitState+`
echo ---
bash "$src/install-units.sh" --helper --enable 2>&1 >/dev/null | grep -c "removed the watchdog"
bash "$src/install-units.sh" --helper --enable 2>&1 >/dev/null | grep -c "removed the watchdog"
echo ---
mv "$src/factory-watchdog.timer" "$src/gone"
bash "$src/install-units.sh" --helper --watchdog --enable 2>&1 >/dev/null; echo "missing $?"
`+unitState, src)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	parts := strings.Split(out, "---\n")
	if len(parts) != 4 {
		t.Fatalf("output:\n%s", out)
	}
	armed := []string{"unit factory-rebuild.service", "unit factory-rebuild.timer", "unit factory-watchdog.service", "unit factory-watchdog.timer",
		"wants factory-rebuild.timer", "wants factory-watchdog.timer", "active factory-rebuild.timer", "active factory-watchdog.timer"}
	if got := stateLines(parts[0]); !slices.Equal(got, armed) {
		t.Errorf("armed twice:\n%s\nwant %q", strings.Join(got, "\n"), armed)
	}
	if !strings.HasPrefix(parts[1], "refused 2\n") || !slices.Equal(stateLines(strings.TrimPrefix(parts[1], "refused 2\n")), armed) {
		t.Errorf("a refused run changed the armed watchdog:\n%s", parts[1])
	}
	if parts[2] != "1\n0\n" {
		t.Errorf("removal notice (first run says it, second has nothing to remove): %q", parts[2])
	}
	if !strings.Contains(parts[3], "factory-watchdog.timer: missing here; re-run host/setup.sh") ||
		!strings.Contains(parts[3], "missing 1\n") || strings.Contains(parts[3], "watchdog.service") {
		t.Errorf("a bundle without the timer:\n%s", parts[3])
	}
}

func TestInstallUnitsWatchdog(t *testing.T) {
	src := unitsDir(t)
	out, code := asRoot(t, unitFakes, []string{"/etc/systemd/system"}, deployedUnits+`
src=$1
bash "$src/install-units.sh" --watchdog --enable >/dev/null 2>&1 && { echo "--watchdog without --helper ran"; exit 1; }
`+unitState+`
echo ---
bash "$src/install-units.sh" --helper --watchdog --enable >/dev/null || { echo "opt-in failed"; exit 1; }
`+unitState+`
echo ---
bash "$src/install-units.sh" --helper --enable >/dev/null || { echo "opt-out failed"; exit 1; }
`+unitState+`
echo ---
bash "$src/install-units.sh" --helper --watchdog >/dev/null || { echo "install without enable failed"; exit 1; }
`+unitState+`
echo ---
bash "$src/install-units.sh" --enable >/dev/null || { echo "dispatcher failed"; exit 1; }
`+unitState, src)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	parts := strings.Split(out, "---\n")
	if len(parts) != 5 {
		t.Fatalf("output:\n%s", out)
	}
	rebuild := []string{"unit factory-rebuild.service", "unit factory-rebuild.timer", "wants factory-rebuild.timer", "active factory-rebuild.timer"}
	for i, want := range [][]string{
		nil,
		{"unit factory-rebuild.service", "unit factory-rebuild.timer", "unit factory-watchdog.service", "unit factory-watchdog.timer",
			"wants factory-rebuild.timer", "wants factory-watchdog.timer", "active factory-rebuild.timer", "active factory-watchdog.timer"},
		rebuild,
		{"unit factory-rebuild.service", "unit factory-rebuild.timer", "unit factory-watchdog.service", "unit factory-watchdog.timer",
			"wants factory-rebuild.timer", "active factory-rebuild.timer"},
	} {
		if got := stateLines(parts[i]); !slices.Equal(got, want) {
			t.Errorf("step %d:\n%s\nwant %q", i+1, strings.Join(got, "\n"), want)
		}
	}
	for _, l := range stateLines(parts[4]) {
		if strings.Contains(l, "watchdog") {
			t.Errorf("the dispatcher kept the watchdog: %s", l)
		}
	}
}

// The fakes allow-puller.sh meets: a factory user whose home is $FAKE/home,
// an sshd that reads <user>.pullers (unless $FAKE/old-sshd exists), and a
// chmod that fails while $FAKE/fail-chmod exists.
var pullerFakes = map[string]string{
	"getent": `#!/bin/bash
[[ $1 == passwd && $2 == factory ]] && echo "factory:x:1001:1001::$FAKE/home:/bin/bash"
`,
	"sshd": `#!/bin/bash
if [[ -e $FAKE/old-sshd ]]; then echo "authorizedkeysfile .ssh/authorized_keys"
else echo "authorizedkeysfile .ssh/authorized_keys /etc/ssh/authorized_keys/%u.pullers"; fi
`,
	"chmod": `#!/bin/bash
[[ -e $FAKE/fail-chmod ]] && { echo "chmod: failed" >&2; exit 1; }
PATH=${PATH#*:} exec chmod "$@" # the real one: the fakes are first on PATH
`,
}

// pullerRun is the body's prelude: a deployed factory-remote, then `p`
// running allow-puller.sh with the given arguments.
const pullerRun = `
mkdir -p "$FAKE/home/factory/bin"
printf '#!/bin/sh\n' >"$FAKE/home/factory/bin/factory-remote"
chmod 755 "$FAKE/home/factory/bin/factory-remote"
k=/etc/ssh/authorized_keys/factory.pullers
p() { bash "$SCRIPT" "$@"; }
`

const (
	keyA = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIA factory-pull"
	keyB = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIB factory-pull"
)

func allowPuller(t *testing.T, body string) (string, int) {
	t.Helper()
	script, err := filepath.Abs("host/allow-puller.sh")
	if err != nil {
		t.Fatal(err)
	}
	return asRoot(t, pullerFakes, []string{"/etc/ssh"}, "SCRIPT='"+script+"'\n"+pullerRun+body, keyA, keyB)
}

// One line per pull host: a re-run replaces that host's line, another
// host's (desktop2, whose name starts like desktop's) is kept, and --undo
// takes out only its own line, then the file once it is empty.
func TestAllowPullerKeepsOneLinePerHost(t *testing.T) {
	out, code := allowPuller(t, `
p --host desktop --from 192.0.2.10 --key "$1" >/dev/null || exit 1
p --host desktop2 --from 192.0.2.11 --key "$2" >/dev/null || exit 1
p --host desktop --from 192.0.2.12 --key "$2" >/dev/null || exit 1
echo "== both"; cat "$k"; stat -c '%a %u' "$k"
p --undo --host desktop >/dev/null || exit 1
echo "== undo"; cat "$k"
p --undo --host desktop >/dev/null || exit 1
p --undo --host desktop2 >/dev/null || exit 1
[[ -e $k ]] && echo "still there"
ls -A /etc/ssh/authorized_keys
true
`)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	remote := "/factory/bin/factory-remote"
	both, undo, _ := strings.Cut(strings.TrimPrefix(out, "== both\n"), "== undo\n")
	if !strings.Contains(both, `restrict,from="192.0.2.11",command="`) || !strings.Contains(both, remote+` mailbox desktop2" `+keyB) ||
		!strings.Contains(both, `restrict,from="192.0.2.12",command="`) || !strings.Contains(both, remote+` mailbox desktop" `+keyB) ||
		strings.Contains(both, "192.0.2.10") || strings.Count(both, "restrict,") != 2 || !strings.Contains(both, "644 0") {
		t.Errorf("after the re-run:\n%s", both)
	}
	if strings.Count(undo, "restrict,") != 1 || !strings.Contains(undo, "mailbox desktop2") || strings.Contains(undo, "still there") ||
		strings.TrimSpace(undo[strings.LastIndex(undo, keyB)+len(keyB):]) != "" {
		t.Errorf("after --undo:\n%s", undo)
	}
}

// A write that fails part way leaves the live file as it was and no
// temporary file beside it.
func TestAllowPullerLeavesNoTempFileOnAFailedWrite(t *testing.T) {
	out, code := allowPuller(t, `
p --host desktop --from 192.0.2.10 --key "$1" >/dev/null || exit 1
touch "$FAKE/fail-chmod"
p --host desktop2 --from 192.0.2.11 --key "$2" && echo "the failing write succeeded"
rm "$FAKE/fail-chmod"
echo "== left"; ls -A /etc/ssh/authorized_keys; cat "$k"
`)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	_, left, _ := strings.Cut(out, "== left\n")
	if strings.Contains(out, "succeeded") || strings.Contains(left, "factory.pullers.") ||
		!strings.HasPrefix(left, "factory.pullers\n") || strings.Count(left, "restrict,") != 1 {
		t.Errorf("after a failed write:\n%s", out)
	}
}

// Until sshd reads .pullers, nothing is written: a key there would not
// work, and the operator would think it did.
func TestAllowPullerRefusesUntilSSHDReadsThePullersFile(t *testing.T) {
	out, code := allowPuller(t, `
touch "$FAKE/old-sshd"
p --host desktop --from 192.0.2.10 --key "$1" && exit 1
[[ -e $k ]] && exit 2
true
`)
	if code != 0 || !strings.Contains(out, "sshd does not read /etc/ssh/authorized_keys/factory.pullers") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

// The units are filled in as root from the system's record of the factory
// user, not taken from the factory user's own rendered copies: an edited
// deployed unit changes nothing, and only the rebuild time comes from the
// deployed timer.
func TestInstallUnitsRendersTheUnitsAsRoot(t *testing.T) {
	src := unitsDir(t)
	out, code := asRoot(t, unitFakes, []string{"/etc/systemd/system"}, deployedUnits+`
printf '[Service]\nUser=root\nExecStart=/bin/sh -c id\n' >"$FAKE/home/factory/systemd/factory@.service"
bash "$1/install-units.sh" >/dev/null || { echo "run failed"; exit 1; }
cat /etc/systemd/system/factory@.service /etc/systemd/system/factory-rebuild.timer
echo "home $FAKE/home"
`, src)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	home := strings.TrimSpace(out[strings.LastIndex(out, "home ")+len("home "):])
	for _, l := range []string{"User=factory", "Group=factory", "After=network-online.target user@1001.service",
		"ExecStart=" + home + "/factory/bin/factory %i", "WorkingDirectory=" + home, "OnCalendar=Mon *-*-* 03:15:00"} {
		if !strings.Contains(out, l+"\n") {
			t.Errorf("no %q in the installed units:\n%s", l, out)
		}
	}
	if strings.Contains(out, "User=root") || strings.Contains(out, "/bin/sh -c id") || strings.Contains(out, "{{") {
		t.Errorf("installed units:\n%s", out)
	}
}

// A copy anyone but root can change, a linked unit, a factory user that is
// root and a rebuild time that is not a plain calendar expression are each
// refused before any unit is installed.
func TestInstallUnitsRefusesWhatRootDoesNotOwnAlone(t *testing.T) {
	src := unitsDir(t)
	out, code := asRoot(t, unitFakes, []string{"/etc/systemd/system"}, deployedUnits+`
src=$1 timer=$FAKE/home/factory/systemd/factory-rebuild.timer
try() { "$@" >/dev/null 2>"$FAKE/err"; echo "exit $? $(tail -1 "$FAKE/err")"; }
chmod g+w "$src/factory@.service"
try bash "$src/install-units.sh"
chmod g-w "$src/factory@.service"
chmod o+w "$src"
try bash "$src/install-units.sh"
chmod o-w "$src"
mv "$src/factory-tick.timer" "$src/tick" && ln -s tick "$src/factory-tick.timer"
try bash "$src/install-units.sh"
rm "$src/factory-tick.timer" && mv "$src/tick" "$src/factory-tick.timer"
try env FACTORY_USER=root bash "$src/install-units.sh"
printf 'OnCalendar=Mon %%u\n' >"$timer"
try bash "$src/install-units.sh"
`+unitState, src)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{
		"exit 1 install-units.sh: factory@.service: not root's alone here",
		"exit 1 install-units.sh: " + src + " is not root's alone",
		"exit 1 install-units.sh: factory-tick.timer: not root's alone here",
		"exit 1 install-units.sh: refusing units that would run as root",
		"factory-rebuild.timer: no plain OnCalendar= line",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if got := stateLines(out); len(got) != 0 {
		t.Errorf("a refused run installed units:\n%s", strings.Join(got, "\n"))
	}
}

// A host script that names its files after NAME must not source
// /etc/os-release in its own shell: os-release sets NAME too ("Ubuntu"),
// which then named every file and comment written after it.
func TestHostScriptsKeepTheirNameFromOSRelease(t *testing.T) {
	paths, err := filepath.Glob("host/*.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if !strings.Contains(s, "NAME=${FACTORY_NAME") {
			continue
		}
		for _, line := range strings.Split(s, "\n") {
			l := strings.TrimSpace(line)
			if strings.HasPrefix(l, ". /etc/os-release") || strings.HasPrefix(l, "source /etc/os-release") {
				t.Errorf("%s sources /etc/os-release in its own shell, which replaces NAME: read what it needs in a subshell", p)
			}
		}
	}
}
