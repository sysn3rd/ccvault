#!/usr/bin/env bash
# Run everything CI runs, in the same order, locally.
#
# Each check reports pass, fail, or skipped-because-a-tool-is-missing. A missing
# tool is never counted as a pass: a green run you did not actually perform is
# worse than a red one.
#
#   ./scripts/qa.sh           # everything
#   ./scripts/qa.sh --quick   # skip the slow checks (race, cross-compile, vuln)
set -uo pipefail

cd "$(dirname "$0")/.."

QUICK=false
[[ "${1:-}" == "--quick" ]] && QUICK=true

pass=0 fail=0 skip=0
failed_names=()

if [[ -t 1 ]]; then
  green=$'\e[32m'; red=$'\e[31m'; yellow=$'\e[33m'; dim=$'\e[2m'; reset=$'\e[0m'
else
  green=""; red=""; yellow=""; dim=""; reset=""
fi

step() {
  local name="$1"; shift
  printf '%s· %s%s ' "$dim" "$name" "$reset"
  local out
  if out=$("$@" 2>&1); then
    printf '%sok%s\n' "$green" "$reset"
    pass=$((pass + 1))
  else
    printf '%sFAILED%s\n' "$red" "$reset"
    printf '%s\n' "$out" | sed 's/^/    /'
    fail=$((fail + 1))
    failed_names+=("$name")
  fi
}

skipped() {
  printf '%s· %s%s %sskipped%s — %s\n' "$dim" "$1" "$reset" "$yellow" "$reset" "$2"
  skip=$((skip + 1))
}

# Prefer a tool on PATH, then the Go bin directory, which is where
# `go install` puts things and which is often not on PATH.
find_tool() {
  local name="$1"
  if command -v "$name" >/dev/null 2>&1; then command -v "$name"; return 0; fi
  local candidate="$(go env GOPATH)/bin/$name"
  [[ -x "$candidate" ]] && { printf '%s' "$candidate"; return 0; }
  candidate="$(go env GOROOT)/bin/$name"
  [[ -x "$candidate" ]] && { printf '%s' "$candidate"; return 0; }
  return 1
}

check_gofmt() {
  local unformatted
  unformatted=$(gofmt -l . | grep -v '^bin/' || true)
  [[ -z "$unformatted" ]] || { printf 'not gofmt-clean:\n%s\n' "$unformatted"; return 1; }
}

check_cross_compile() {
  local target
  for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
    GOOS="${target%/*}" GOARCH="${target#*/}" go build -o /dev/null ./cmd/ccvault || return 1
  done
}

echo "ccvault QA"
echo

step "build          " go build ./...
step "gofmt          " check_gofmt
step "go vet         " go vet ./...

if tool=$(find_tool staticcheck); then
  step "staticcheck    " "$tool" ./...
else
  skipped "staticcheck    " "go install honnef.co/go/tools/cmd/staticcheck@latest"
fi

step "tests          " go test ./...

if [[ "$QUICK" == false ]]; then
  step "race detector  " go test -race ./...

  if tool=$(find_tool govulncheck); then
    step "govulncheck    " "$tool" ./...
  else
    skipped "govulncheck    " "go install golang.org/x/vuln/cmd/govulncheck@latest"
  fi

  step "cross-compile  " check_cross_compile
else
  skipped "race detector  " "--quick"
  skipped "govulncheck    " "--quick"
  skipped "cross-compile  " "--quick"
fi

echo
echo "coverage"
./scripts/coverage.sh | sed 's/^/  /'

echo
if (( fail > 0 )); then
  printf '%s%d failed%s, %d passed, %d skipped: %s\n' \
    "$red" "$fail" "$reset" "$pass" "$skip" "${failed_names[*]}"
  exit 1
fi
printf '%s%d passed%s, %d skipped\n' "$green" "$pass" "$reset" "$skip"
