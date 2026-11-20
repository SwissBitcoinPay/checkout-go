package checkout_test

import (
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// What the README says that its text alone cannot keep true: its Go blocks are Go, where
// golang.org/x/sys is built into the package, and what of the secp256k1 library is not
// constant-time. The go command answers offline (GOPROXY=off): from the module cache, as the
// package and its tests were built.

func readme(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("README.md")
	must(t, err)
	return string(data)
}

// goCommand is what the go command says of args, its environment env more, offline.
func goCommand(t *testing.T, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Env = append(append(os.Environ(), "GOPROXY=off"), env...)
	out, err := cmd.Output()
	if err != nil {
		var stderr []byte
		if exit, ok := err.(*exec.ExitError); ok {
			stderr = exit.Stderr
		}
		t.Fatalf("go %s: %v: %s", strings.Join(args, " "), err, stderr)
	}
	return string(out)
}

// paragraphOf is the README's paragraph (between blank lines) that holds text, its lines joined.
func paragraphOf(t *testing.T, text string) string {
	t.Helper()
	for _, paragraph := range strings.Split(readme(t), "\n\n") {
		if paragraph = strings.Join(strings.Fields(paragraph), " "); strings.Contains(paragraph, text) {
			return paragraph
		}
	}
	t.Fatalf("no paragraph of the README holds %q", text)
	return ""
}

// Each Go block of the README is Go: a file's declarations (an import, functions), or a function's
// statements; never both at once, which no Go source holds.
func TestTheReadmesGoBlocksAreGo(t *testing.T) {
	blocks := regexp.MustCompile("(?s)\n```go\n(.*?)\n```\n").FindAllStringSubmatch(readme(t), -1)
	if len(blocks) < 5 {
		t.Fatalf("%d Go blocks", len(blocks))
	}
	for i, block := range blocks {
		fset := token.NewFileSet()
		_, asFile := parser.ParseFile(fset, "declarations.go", "package readme\n"+block[1], parser.AllErrors)
		_, asBody := parser.ParseFile(fset, "statements.go", "package readme\nfunc _() {\n"+block[1]+"\n}\n", parser.AllErrors)
		if asFile != nil && asBody != nil {
			t.Errorf("Go block %d is neither declarations (%v) nor statements (%v):\n%s", i+1, asFile, asBody, block[1])
		}
	}
}

// The README names the architectures where a package of golang.org/x/sys is built into this one
// (golang.org/x/crypto's ChaCha20 asks its package cpu about the processor on some), as the go
// command lists the package's dependencies for each architecture Go builds for Linux.
func TestTheReadmeSaysWhereGolangOrgXSysIsBuiltIn(t *testing.T) {
	var architectures []string
	for _, platform := range strings.Fields(goCommand(t, nil, "tool", "dist", "list")) {
		if goos, goarch, _ := strings.Cut(platform, "/"); goos == "linux" {
			architectures = append(architectures, goarch)
		}
	}
	if len(architectures) < 10 {
		t.Fatalf("architectures %v", architectures)
	}
	var builtIn, named []string
	paragraph := paragraphOf(t, "`golang.org/x/sys`")
	for _, goarch := range architectures {
		// -e: a package listed though its module is not in the cache (it is never downloaded).
		deps := goCommand(t, []string{"GOOS=linux", "GOARCH=" + goarch, "CGO_ENABLED=0"}, "list", "-e", "-deps", "-f", "{{.ImportPath}}", ".")
		if slices.ContainsFunc(strings.Fields(deps), func(p string) bool { return strings.HasPrefix(p, "golang.org/x/sys/") }) {
			builtIn = append(builtIn, goarch)
		}
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(goarch) + `\b`).MatchString(paragraph) {
			named = append(named, goarch)
		}
	}
	if !slices.Equal(named, builtIn) {
		t.Errorf("golang.org/x/sys is built in on %v; the README names %v:\n%s", builtIn, named, paragraph)
	}
}

// SPEC.md §27: the README says whether the secp256k1 library signs (BIP-340) and computes ECDH
// (NIP-44) in constant time. It says it does not, naming the library's versions and the functions
// that multiply a point by a secret in variable time; the test reads the pinned library's source,
// where those functions do so.
func TestTheReadmeSaysWhatOfSecp256k1IsNotConstantTime(t *testing.T) {
	paragraph := paragraphOf(t, "**Signing and ECDH are not constant-time.**")
	modules := map[string]string{"github.com/btcsuite/btcd/btcec/v2": "", "github.com/decred/dcrd/dcrec/secp256k1/v4": ""}
	for module := range modules {
		fields := strings.Fields(goCommand(t, nil, "list", "-m", "-f", "{{.Version}} {{.Dir}}", module))
		if len(fields) != 2 {
			t.Fatalf("%s: %v", module, fields)
		}
		modules[module] = fields[1]
		if !strings.Contains(paragraph, "`"+module+"` "+fields[0]) {
			t.Errorf("the README does not name %s %s", module, fields[0])
		}
	}
	for _, c := range []struct {
		module, file, call string
		// named is what the README names of it: the function, and its file when it does.
		named []string
	}{
		// A signature's nonce point, in BIP-340's step 10.
		{"github.com/btcsuite/btcd/btcec/v2", "schnorr/signature.go", "btcec.ScalarBaseMultNonConst(&k, &R)", []string{"`ScalarBaseMultNonConst`", "`schnorr/signature.go`"}},
		// A public key of a secret.
		{"github.com/decred/dcrd/dcrec/secp256k1/v4", "privkey.go", "ScalarBaseMultNonConst(&p.Key, &result)", []string{"`ScalarBaseMultNonConst`"}},
		// An ECDH.
		{"github.com/decred/dcrd/dcrec/secp256k1/v4", "ecdh.go", "ScalarMultNonConst(&privkey.Key, &point, &result)", []string{"`ScalarMultNonConst`", "`ecdh.go`"}},
	} {
		source, err := os.ReadFile(filepath.Join(modules[c.module], filepath.FromSlash(c.file)))
		must(t, err)
		if !strings.Contains(string(source), c.call) {
			t.Errorf("%s no longer calls %s: is it constant-time now? Say so in the README", c.file, c.call)
		}
		for _, name := range c.named {
			if !strings.Contains(paragraph, name) {
				t.Errorf("the README does not name %s (%s)", name, c.file)
			}
		}
	}
}
