package specpin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sattleme/checkout-go/internal/testvec"
)

// The pinned copy of the specification (spec/, PORTING.md of checkout-spec): every file of
// spec/PIN.json is there with its SHA-256, and nothing else. It changes only by taking a newer
// version of checkout-spec (go run ./internal/cmd/spec import <clone>), never by hand.
func TestTheCopyIsThePinnedOne(t *testing.T) {
	if problems := Check(filepath.Join(testvec.Root(), "spec")); len(problems) > 0 {
		t.Errorf("spec/ is not PIN.json's:\n%s", strings.Join(problems, "\n"))
	}
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=Test", "-c", "user.email=test@sattle.me", "-c", "commit.gpgsign=false"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// A clone of a specification whose manifest lists two files: imported, then checked.
func TestImportThenCheck(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	clone := t.TempDir()
	write(t, filepath.Join(clone, "SPEC.md"), "# spec\n")
	write(t, filepath.Join(clone, "vectors", "a.json"), "{}\n")
	write(t, filepath.Join(clone, "manifest.json"), `{
  "version": "9.9.9",
  "protocol": 1,
  "files": {
    "SPEC.md": "1b355baa30768cee67e2f1f9ff2d45584cb42f9413881d9104579d26cff1d2e8",
    "vectors/a.json": "ca3d163bab055381827226140568f3bef7eaac187cebd76878e0b63e9e442356"
  }
}
`)
	run(t, clone, "init", "-q")
	run(t, clone, "add", ".")
	run(t, clone, "commit", "-q", "-m", "Spec")
	copyDir := filepath.Join(t.TempDir(), "spec")
	write(t, filepath.Join(copyDir, "old.md"), "dropped\n")
	commit, version, err := Import(clone, copyDir)
	if err != nil {
		t.Fatal(err)
	}
	if version != "9.9.9" || len(commit) != 40 {
		t.Errorf("commit %s, version %s", commit, version)
	}
	if problems := Check(copyDir); len(problems) > 0 {
		t.Errorf("after the import: %v", problems)
	}
	pin, _ := os.ReadFile(filepath.Join(copyDir, PinFile))
	want := "{\n  \"repository\": \"https://github.com/sattleme/checkout-spec\",\n  \"commit\": \"" + commit +
		"\",\n  \"version\": \"9.9.9\",\n  \"protocol\": 1,\n  \"files\": {\n" +
		"    \"SPEC.md\": \"1b355baa30768cee67e2f1f9ff2d45584cb42f9413881d9104579d26cff1d2e8\",\n" +
		"    \"vectors/a.json\": \"ca3d163bab055381827226140568f3bef7eaac187cebd76878e0b63e9e442356\"\n  }\n}\n"
	if string(pin) != want {
		t.Errorf("PIN.json:\n%s\nwant\n%s", pin, want)
	}

	// A file changed, missing or added is found.
	write(t, filepath.Join(copyDir, "SPEC.md"), "# changed\n")
	write(t, filepath.Join(copyDir, "extra.json"), "{}")
	os.Remove(filepath.Join(copyDir, "vectors", "a.json"))
	problems := strings.Join(Check(copyDir), "\n")
	for _, want := range []string{"SPEC.md differs from PIN.json", "vectors/a.json is missing", "extra.json is not in PIN.json"} {
		if !strings.Contains(problems, want) {
			t.Errorf("%q not found in:\n%s", want, problems)
		}
	}

	// A clone that is not clean, or whose files are not its manifest's, is refused.
	write(t, filepath.Join(clone, "SPEC.md"), "# edited\n")
	if _, _, err := Import(clone, copyDir); err == nil {
		t.Error("an unclean clone imported")
	}
	run(t, clone, "commit", "-q", "-am", "Edit")
	if _, _, err := Import(clone, copyDir); err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Errorf("a clone off its manifest imported: %v", err)
	}
}

// PORTING.md, definition of done, 1: every conformance vector is passed, so every vector file of
// the pinned copy is read by a test of the repository (by its path under spec/vectors, quoted),
// and the currency table by the package's own (internal/currencies).
func TestEveryVectorFileIsReadByATest(t *testing.T) {
	root := testvec.Root()
	var sources []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "spec") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(path, "_test.go") && !strings.HasSuffix(path, filepath.Join("specpin", "specpin_test.go")) {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sources = append(sources, string(data))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	read := func(name string) bool {
		for _, s := range sources {
			if strings.Contains(s, `"`+name+`"`) {
				return true
			}
		}
		return false
	}
	vectors := filepath.Join(root, "spec", "vectors")
	files := 0
	err = filepath.WalkDir(vectors, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		rel, _ := filepath.Rel(vectors, path)
		if name := filepath.ToSlash(rel); !read(name) {
			t.Errorf("no test reads spec/vectors/%s", name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("no vector files")
	}
	if !read("../currencies.json") {
		t.Error("no test reads spec/currencies.json")
	}
	t.Logf("%d vector files", files)
}
