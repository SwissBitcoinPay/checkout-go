// Command spec keeps the pinned copy of the specification (spec/, PORTING.md of checkout-spec:
// "The pinned copy of the specification").
//
//	go run ./internal/cmd/spec check
//	    Whether spec/ is PIN.json's, file for file (the tests run the same check).
//	go run ./internal/cmd/spec import <checkout-spec clone>
//	    Replaces spec/ with the files of a clean clone, at the commit it is on, and writes
//	    PIN.json. Its manifest.json must be its files'. Clone and check out the version yourself:
//	    no network.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/sattleme/checkout-go/internal/specpin"
)

func main() {
	wd, err := os.Getwd()
	if err != nil {
		fail(err)
	}
	root, err := specpin.RepositoryRoot(wd)
	if err != nil {
		fail(err)
	}
	copyDir := filepath.Join(root, "spec")
	switch {
	case len(os.Args) == 2 && os.Args[1] == "check":
		problems := specpin.Check(copyDir)
		for _, problem := range problems {
			fmt.Fprintln(os.Stderr, problem)
		}
		if len(problems) > 0 {
			os.Exit(1)
		}
	case len(os.Args) == 3 && os.Args[1] == "import":
		commit, version, err := specpin.Import(os.Args[2], copyDir)
		if err != nil {
			fail(err)
		}
		fmt.Printf("spec/ is checkout-spec %s at %s\n", version, commit)
	default:
		fmt.Fprintln(os.Stderr, "Usage: go run ./internal/cmd/spec check | import <checkout-spec clone>")
		os.Exit(2)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
