// Package specpin keeps the pinned copy of the specification (sattleme/checkout-spec, its
// PORTING.md: "The pinned copy of the specification"): spec/ holds the files its manifest.json
// lists, unchanged, and spec/PIN.json says which commit they come from, with the SHA-256 of each.
package specpin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/sattleme/checkout-go/internal/jsonv"
)

// Repository is the specification's repository.
const Repository = "https://github.com/sattleme/checkout-spec"

// PinFile is the name of the pin, in the copy.
const PinFile = "PIN.json"

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func sha256File(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// filesUnder lists every file under dir, as paths relative to it with "/", sorted.
func filesUnder(dir string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			found = append(found, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(found)
	return found, err
}

func readJSON(path string) (*jsonv.Object, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	v, err := jsonv.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	o, ok := jsonv.AsObject(v)
	if !ok {
		return nil, fmt.Errorf("%s: not an object", filepath.Base(path))
	}
	return o, nil
}

// Check lists what differs between the copy and its PIN.json: nothing when the copy is the
// pinned one, file for file.
func Check(copyDir string) []string {
	pin, err := readJSON(filepath.Join(copyDir, PinFile))
	if errors.Is(err, fs.ErrNotExist) {
		return []string{PinFile + " is missing"}
	}
	if err != nil {
		return []string{err.Error()}
	}
	var problems []string
	if repository, _ := pin.Get("repository"); repository != Repository {
		problems = append(problems, PinFile+": repository is not "+Repository)
	}
	commit, _ := pin.Get("commit")
	if s, ok := commit.(string); !ok || !commitPattern.MatchString(s) {
		problems = append(problems, PinFile+": commit is not a commit")
	}
	filesValue, _ := pin.Get("files")
	files, ok := jsonv.AsObject(filesValue)
	if !ok {
		files = jsonv.NewObject()
	}
	for _, file := range files.Keys() {
		want, _ := files.Get(file)
		got, err := sha256File(filepath.Join(copyDir, filepath.FromSlash(file)))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			problems = append(problems, file+" is missing")
		case err != nil:
			problems = append(problems, file+": "+err.Error())
		case got != want:
			problems = append(problems, file+" differs from "+PinFile)
		}
	}
	present, err := filesUnder(copyDir)
	if err != nil {
		return append(problems, err.Error())
	}
	for _, file := range present {
		if file != PinFile && !files.Has(file) {
			problems = append(problems, file+" is not in "+PinFile)
		}
	}
	return problems
}

func git(clone string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", clone}, args...)...)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// Import replaces the copy with the files of a clean clone of the specification, at the commit
// it is on, and writes PIN.json. The clone's manifest.json must be its files'. No network: the
// clone is checked out on the version to take beforehand.
func Import(clone, copyDir string) (commit, version string, err error) {
	status, err := git(clone, "status", "--porcelain")
	if err != nil {
		return "", "", err
	}
	if status != "" {
		return "", "", fmt.Errorf("%s is not clean", clone)
	}
	commit, err = git(clone, "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	manifest, err := readJSON(filepath.Join(clone, "manifest.json"))
	if err != nil {
		return "", "", err
	}
	filesValue, _ := manifest.Get("files")
	files, ok := jsonv.AsObject(filesValue)
	if !ok {
		return "", "", errors.New("manifest.json: no files")
	}
	for _, file := range files.Keys() {
		want, _ := files.Get(file)
		got, err := sha256File(filepath.Join(clone, filepath.FromSlash(file)))
		if err != nil {
			return "", "", err
		}
		if got != want {
			return "", "", fmt.Errorf("%s is not the manifest's", file)
		}
	}
	if err := os.RemoveAll(copyDir); err != nil {
		return "", "", err
	}
	for _, file := range files.Keys() {
		data, err := os.ReadFile(filepath.Join(clone, filepath.FromSlash(file)))
		if err != nil {
			return "", "", err
		}
		target := filepath.Join(copyDir, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", "", err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return "", "", err
		}
	}
	versionValue, _ := manifest.Get("version")
	protocol, _ := manifest.Get("protocol")
	pin := jsonv.Members{
		{Name: "repository", Value: Repository},
		{Name: "commit", Value: commit},
		{Name: "version", Value: versionValue},
		{Name: "protocol", Value: protocol},
		{Name: "files", Value: files},
	}
	// As JSON.stringify(pin, null, 2) writes it, as every implementation's import does.
	text := append(jsonv.Indent(pin, "  "), '\n')
	if err := os.WriteFile(filepath.Join(copyDir, PinFile), text, 0o644); err != nil {
		return "", "", err
	}
	version, _ = versionValue.(string)
	return commit, version, nil
}

// RepositoryRoot finds the repository's root: the directory of go.mod, from dir up.
func RepositoryRoot(dir string) (string, error) {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found")
		}
		dir = parent
	}
}
