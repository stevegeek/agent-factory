package agentfactory

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// Every file under the embedded directories on disk is in Assets with the
// same bytes, dotfiles included.
func TestAssetsMatchTheTree(t *testing.T) {
	for _, dir := range []string{"templates", "systemd", "images", "skills", "examples"} {
		n := 0
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			n++
			want, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			got, err := fs.ReadFile(Assets, filepath.ToSlash(path))
			if err != nil {
				t.Errorf("%s not embedded: %v", path, err)
				return nil
			}
			if string(got) != string(want) {
				t.Errorf("%s: embedded bytes differ from the file", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Errorf("%s: no files found", dir)
		}
	}
}

func TestAssetsHoldNothingElse(t *testing.T) {
	entries, err := fs.ReadDir(Assets, ".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"examples", "images", "skills", "systemd", "templates"}
	if len(names) != len(want) {
		t.Fatalf("top level = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("top level = %v, want %v", names, want)
		}
	}
}
