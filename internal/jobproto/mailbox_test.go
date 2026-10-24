package jobproto

import (
	"archive/tar"
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMailArgv(t *testing.T) {
	argv, err := MailArgv("ssh", "hub", "/k", MailClaim, "desktop", "")
	want := []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "IdentitiesOnly=yes", "-i", "/k", "hub", "claim", "desktop"}
	if err != nil || !reflect.DeepEqual(argv, want) {
		t.Errorf("claim: %q %v", argv, err)
	}
	argv, err = MailArgv("/usr/bin/ssh", "factory@hub", "", MailBeat, "desktop", "t0001-w1")
	want = []string{"/usr/bin/ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "factory@hub", "beat", "desktop", "t0001-w1"}
	if err != nil || !reflect.DeepEqual(argv, want) {
		t.Errorf("beat: %q %v", argv, err)
	}
	argv, err = MailArgv("ssh", "hub", "", MailVitals, "desktop", "")
	want = []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "hub", "vitals", "desktop"}
	if err != nil || !reflect.DeepEqual(argv, want) {
		t.Errorf("vitals: %q %v", argv, err)
	}
	for _, bad := range [][4]string{
		{"hub", MailVitals, "desktop", "t0001-w1"},
		{"-oProxyCommand=x", MailClaim, "desktop", ""},
		{"", MailClaim, "desktop", ""},
		{"hub", "start", "desktop", "t0001-w1"},
		{"hub", MailClaim, "../x", ""},
		{"hub", MailClaim, "desktop", "t0001-w1"},
		{"hub", MailDone, "desktop", "T1"},
		{"hub", MailRelease, "desktop", ""},
	} {
		if _, err := MailArgv("ssh", bad[0], "", bad[1], bad[2], bad[3]); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if MailTimeout(MailDone) != 300*time.Second || MailTimeout(MailBeat) != 60*time.Second {
		t.Error("timeouts")
	}
}

func TestPackCollectOrdersCapsAndCutsTheLog(t *testing.T) {
	log := bytes.Repeat([]byte("x"), 2_000_010)
	b, err := PackCollect(map[string][]byte{
		MemberOutBundle: []byte("bundle"), MemberReport: []byte(`{"status": "done"}`), MemberHead: []byte("abc\n"),
		MemberLog: log, "stray": []byte("no"), MemberMemoryTar: bytes.Repeat([]byte("m"), 4_000_001),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckCollect(b); err != nil {
		t.Fatalf("its own tar: %v", err)
	}
	var names []string
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
		if h.Name == MemberLog && h.Size != 2_000_000 {
			t.Errorf("agent.log size %d", h.Size)
		}
	}
	if want := []string{MemberReport, MemberHead, MemberLog, MemberOutBundle}; !reflect.DeepEqual(names, want) {
		t.Errorf("members %q", names)
	}
	files, err := UntarFlat(b)
	if err != nil || string(files[MemberHead]) != "abc\n" {
		t.Errorf("untar: %v %q", err, files[MemberHead])
	}
}

func TestCheckCollectRefuses(t *testing.T) {
	mk := func(hs ...tar.Header) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, h := range hs {
			tw.WriteHeader(&h)
			if h.Typeflag == tar.TypeReg {
				tw.Write(make([]byte, h.Size))
			}
		}
		tw.Close()
		return buf.Bytes()
	}
	reg := func(name string, size int64) tar.Header {
		return tar.Header{Typeflag: tar.TypeReg, Name: name, Size: size, Mode: 0o644}
	}
	for name, tc := range map[string]struct {
		b    []byte
		want string
	}{
		"empty":          {nil, "empty"},
		"garbage":        {[]byte("not a tar at all, not a tar at all"), "collect tar"},
		"unknown member": {mk(reg("id_ed25519", 3)), "not a collect member"},
		"path":           {mk(reg("x/report.json", 3)), "not a collect member"},
		"symlink":        {mk(tar.Header{Typeflag: tar.TypeSymlink, Name: "head", Linkname: "/etc/passwd"}), "not a regular file"},
		"dir":            {mk(tar.Header{Typeflag: tar.TypeDir, Name: "report.json"}), "not a regular file"},
		"over its cap":   {mk(reg("head", 1001)), "over 1000 bytes"},
		"twice":          {mk(reg("head", 1), reg("head", 1)), "twice"},
		"cut short":      {mk(reg("report.json", 10))[:515], "cut short"},
	} {
		if err := CheckCollect(tc.b); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}
