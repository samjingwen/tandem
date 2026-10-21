#!/bin/sh
# Execute the actual quality consumers against disposable conforming/violating
# fixtures. This is a Lint-phase proof, not a source-text test.
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
probe=$(mktemp -d "${TMPDIR:-/tmp}/tandem-quality.XXXXXX")
trap 'rm -f "$probe/pass.go" "$probe/vet.go" "$probe/smell.go" "$probe/vet.log" "$probe/static.log" "$probe/format.log" "$probe/core/format.go"; rmdir "$probe/core" "$probe/internal" "$probe/cmd" "$probe"' EXIT
mkdir "$probe/core" "$probe/internal" "$probe/cmd"
cat > "$probe/pass.go" <<'EOF'
package main

func main() {}
EOF
cat > "$probe/vet.go" <<'EOF'
package main

import "fmt"

func main() { fmt.Printf("%d", "wrong argument type") }
EOF
cat > "$probe/smell.go" <<'EOF'
package main

import "time"

func main() { time.Sleep(1) }
EOF
cat > "$probe/core/format.go" <<'EOF'
package probe
func Example( ){ }
EOF
cd "$root"
go vet "$probe/pass.go"
go tool staticcheck "$probe/pass.go"
if go vet "$probe/vet.go" > "$probe/vet.log" 2>&1; then
  echo 'vet accepted a format-argument bug' >&2
  exit 1
fi
if go tool staticcheck "$probe/smell.go" > "$probe/static.log" 2>&1; then
  echo 'staticcheck accepted a nanosecond sleep mistake' >&2
  exit 1
fi
if make -s -C "$probe" -f "$root/Makefile" format-check > "$probe/format.log" 2>&1; then
  echo 'format-check accepted unformatted maintained Go' >&2
  exit 1
fi
gofmt -w "$probe/core/format.go"
make -s -C "$probe" -f "$root/Makefile" format-check
echo 'quality enforcement: conforming code passes; formatting, vet, and staticcheck violations fail'
