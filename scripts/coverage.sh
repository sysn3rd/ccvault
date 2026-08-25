#!/usr/bin/env bash
# Measure test coverage honestly.
#
# Two adjustments are needed for the number to mean anything here:
#
#   -coverpkg=./...  attributes coverage across package boundaries. Without it,
#                    internal/index reads as 0% despite being exercised by every
#                    other package's tests.
#
#   GOCOVERDIR       collects coverage from the ccvault binary that the CLI
#                    tests run as a subprocess. Without it, every command
#                    handler reads as untested even though those tests drive
#                    all of them.
#
# Naive `go test -cover ./...` reports roughly 44%; the real figure is closer to 68%.
set -euo pipefail

cd "$(dirname "$0")/.."

covdir=$(mktemp -d)
unit=$(mktemp)
sub=$(mktemp)
trap 'rm -rf "$covdir" "$unit" "$sub"' EXIT

# -count=1 defeats the test cache: a cached result runs no test binary, so the
# subprocess coverage files are never written and the CLI reads as untested.
CCVAULT_COVERDIR="$covdir" go test -count=1 -coverpkg=./... -coverprofile="$unit" ./... >/dev/null
go tool covdata textfmt -i="$covdir" -o="$sub" 2>/dev/null || : > "$sub"

python3 - "$unit" "$sub" "${1:-}" <<'PY'
import collections, re, sys

unit, sub, out_path = sys.argv[1], sys.argv[2], sys.argv[3]
blocks, stmts, mode = collections.defaultdict(int), {}, "set"

for path in (unit, sub):
    try:
        for line in open(path):
            line = line.strip()
            if line.startswith("mode:"):
                mode = line.split()[1]
                continue
            m = re.match(r"^(.+):(\d+\.\d+,\d+\.\d+) (\d+) (\d+)$", line)
            if not m:
                continue
            key = (m.group(1), m.group(2))
            stmts[key] = int(m.group(3))
            blocks[key] += int(m.group(4))
    except FileNotFoundError:
        pass

if not stmts:
    sys.exit("no coverage data produced")

if out_path:
    with open(out_path, "w") as f:
        f.write(f"mode: {mode}\n")
        for (name, rng), count in blocks.items():
            f.write(f"{name}:{rng} {stmts[(name, rng)]} {count}\n")

per_pkg = collections.defaultdict(lambda: [0, 0])
for key, count in blocks.items():
    pkg = "/".join(key[0].split("/")[3:-1]) or "root"
    per_pkg[pkg][1] += stmts[key]
    if count > 0:
        per_pkg[pkg][0] += stmts[key]

for pkg, (cov, tot) in sorted(per_pkg.items(), key=lambda kv: -kv[1][0] / max(kv[1][1], 1)):
    print(f"  {cov / tot * 100:5.1f}%  {pkg}")

total = sum(stmts.values())
covered = sum(stmts[k] for k, c in blocks.items() if c > 0)
print(f"\n  TOTAL {covered / total * 100:.1f}%  ({covered}/{total} statements)")
PY
