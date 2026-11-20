#!/usr/bin/env bash
# Every check of the repository, as the CI runs them: `scripts/check.sh` runs them all, in this
# order; `scripts/check.sh <name>…` runs those named. The tools are pinned here, and run with the Go
# toolchain on PATH (go.mod: Go 1.26.10 or later).
#
#   spec    spec/ is the pinned copy of checkout-spec (spec/PIN.json), file for file
#   format  every Go file is gofmt's
#   vet     go vet, here and for a 32-bit architecture (386: an int of 32 bits), tests included
#   lint    staticcheck
#   mod     go.mod and go.sum are tidy, and the modules downloaded are go.sum's
#   test    every test, with the race detector (it needs cgo: a C compiler)
#   build   the package and its tools build
#   vuln    govulncheck: known vulnerabilities of the code's dependencies (asks vuln.go.dev)
set -euo pipefail

STATICCHECK="honnef.co/go/tools/cmd/staticcheck@v0.8.2"
GOVULNCHECK="golang.org/x/vuln/cmd/govulncheck@v1.9.0"

cd "$(dirname "$0")/.."

run() {
  case "$1" in
    spec) go run ./internal/cmd/spec check ;;
    format)
      unformatted="$(gofmt -l .)"
      if [ -n "$unformatted" ]; then
        echo "Not formatted (gofmt -w):" >&2
        echo "$unformatted" >&2
        return 1
      fi
      ;;
    vet)
      go vet ./...
      GOARCH=386 go vet ./...
      ;;
    lint) go run "$STATICCHECK" ./... ;;
    mod)
      go mod tidy -diff
      go mod verify
      ;;
    test) go test -race -count=1 ./... ;;
    build) go build ./... ;;
    vuln) go run "$GOVULNCHECK" ./... ;;
    *)
      echo "Unknown check: $1" >&2
      return 2
      ;;
  esac
}

if [ "$#" -eq 0 ]; then
  set -- spec format vet lint mod test build vuln
fi
for check in "$@"; do
  echo "== $check"
  run "$check"
done
