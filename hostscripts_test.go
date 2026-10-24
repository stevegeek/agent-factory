package agentfactory

// The root scripts (systemd/install-units.sh, host/allow-puller.sh) run
// here as root in a user namespace of their own, with a tmpfs over the
// directories they write (/etc/systemd/system, /etc/ssh) and fakes for
// systemctl, sshd and getent on PATH. Nothing on this machine changes; the
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

// unitsDir is systemd/ copied with install-units.sh rendered for a factory
// user that is this namespace's root (uid 0).
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
	s := readFile(t, filepath.Join(dir, "install-units.sh"))
	s = strings.NewReplacer("{{factory_user}}", "root", "{{factory_uid}}", "0").Replace(s)
	mkexec(t, filepath.Join(dir, "install-units.sh"), []byte(s))
	return dir
}

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
	out, code := asRoot(t, map[string]string{"systemctl": fakeSystemctl}, []string{"/etc/systemd/system"}, `
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
	out, code := asRoot(t, map[string]string{"systemctl": fakeSystemctl}, []string{"/etc/systemd/system"}, `
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
