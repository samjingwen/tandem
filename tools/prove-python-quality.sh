#!/bin/sh
# Exercise the real Makefile Python checks without running the safety consumer.
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
RUFF=${RUFF:-"$root/.tmp/python-tools/bin/ruff"}
mkdir -p "$root/.tmp"
fixture=$(mktemp -d "$root/.tmp/python-quality.XXXXXX")
mkdir "$fixture/tools"
cp "$root/ruff.toml" "$fixture/ruff.toml"
check() {
    make -s -C "$fixture" -f "$root/Makefile" RUFF="$RUFF" "$@"
}
cat > "$fixture/tools/test-engine-safety.py" <<'EOF'
import json

print(json.dumps({"conforming": True}))
EOF
check python-lint > "$fixture/conforming.log" 2>&1
cat > "$fixture/tools/test-engine-safety.py" <<'EOF'
print(  "unformatted"  )
EOF
if check python-format-check > "$fixture/rejected-format.log" 2>&1; then
    echo "Ruff accepted an unformatted consumer" >&2
    exit 1
fi
grep -q 'Would reformat' "$fixture/rejected-format.log"
check python-format > "$fixture/fixed-format.log" 2>&1
check python-lint > "$fixture/fixed-check.log" 2>&1
cat > "$fixture/tools/test-engine-safety.py" <<'EOF'
import json

print(undefined_name)
EOF
if check python-lint > "$fixture/rejected-lint.log" 2>&1; then
    echo "Ruff accepted unused imports and undefined names" >&2
    exit 1
fi
grep -q 'F401' "$fixture/rejected-lint.log"
grep -q 'F821' "$fixture/rejected-lint.log"
cat > "$fixture/tools/test-engine-safety.py" <<'EOF'
import json
import argparse

print(argparse.__name__, json.__name__)
EOF
if check python-lint > "$fixture/rejected-imports.log" 2>&1; then
    echo "Ruff accepted unsorted imports" >&2
    exit 1
fi
grep -q 'I001' "$fixture/rejected-imports.log"
check python-format > "$fixture/fixed-imports.log" 2>&1
check python-lint > "$fixture/final-check.log" 2>&1
echo "Python formatting/lint rejected real violations; conforming consumers passed. Evidence: $fixture"
