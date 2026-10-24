package jobtail

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMailTailReadsTheLastBeatsLog(t *testing.T) {
	dir := t.TempDir()
	jd := filepath.Join(dir, "desktop", "t0001-w1")
	if err := os.MkdirAll(jd, 0o700); err != nil {
		t.Fatal(err)
	}
	if s, err := MailTail(dir, "desktop", "t0001-w1"); s != "" || err != nil {
		t.Errorf("no beat yet: %q %v", s, err)
	}
	if s, err := MailTail(dir, "desktop", "t0009-w1"); s != "" || err != nil {
		t.Errorf("no job: %q %v", s, err)
	}
	if err := os.WriteFile(filepath.Join(jd, "beat.log"), []byte("step 3 \xe2\x82"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := MailTail(dir, "desktop", "t0001-w1"); s != "step 3 �" || err != nil {
		t.Errorf("tail: %q %v", s, err)
	}
	for _, bad := range [][2]string{{"..", "t0001-w1"}, {"desktop", "../x"}} {
		if _, err := MailTail(dir, bad[0], bad[1]); err == nil {
			t.Errorf("%q read", bad)
		}
	}
	os.Remove(filepath.Join(jd, "beat.log"))
	if err := os.Symlink("/etc/passwd", filepath.Join(jd, "beat.log")); err != nil {
		t.Fatal(err)
	}
	if _, err := MailTail(dir, "desktop", "t0001-w1"); err == nil {
		t.Error("read through a link")
	}
}
