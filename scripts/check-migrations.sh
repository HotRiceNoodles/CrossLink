#!/usr/bin/env bash
# Structural validation for migrations/ directories.
#
# Checks (per dialect directory, e.g. migrations/postgres):
#   1. Every NNNNNN_name.up.sql has a matching .down.sql (and vice versa)
#   2. Sequence numbers are unique
#   3. Sequence numbers have no gaps (0-padding is 6 digits by convention)
#
# Exit code 1 on any violation. POSIX bash — runs in git bash on Windows.
set -euo pipefail

cd "$(dirname "$0")/.."

fail=0

for dir in migrations/*/; do
    dialect=$(basename "$dir")
    nums=""

    while IFS= read -r file; do
        name=$(basename "$file")
        num=$(echo "$name" | grep -oE '^[0-9]{6}' || true)
        if [ -z "$num" ]; then
            echo "ERROR [$dialect]: $name does not match NNNNNN_snake_name pattern"
            fail=1
            continue
        fi
        case "$name" in
            *.up.sql)   kind="up" ;;
            *.down.sql) kind="down" ;;
            *)
                echo "ERROR [$dialect]: $name is neither .up.sql nor .down.sql"
                fail=1
                continue
                ;;
        esac
        base="${name%.$kind.sql}"  # strip .up/.down.sql -> NNNNNN_name
        pair="migrations/$dialect/${base}.$( [ "$kind" = "up" ] && echo down || echo up ).sql"
        if [ ! -f "$pair" ]; then
            echo "ERROR [$dialect]: $name is missing its $pair counterpart"
            fail=1
        fi
        # Count each migration once (via its up file) for duplicate/gap checks.
        if [ "$kind" = "up" ]; then
            nums="$nums $num "
        fi
    done < <(find "$dir" -maxdepth 1 -name '*.sql' | sort)

    # Uniqueness + gaps
    sorted=$(echo "$nums" | tr ' ' '\n' | grep -E '^[0-9]+$' | sort)
    dupes=$(echo "$sorted" | uniq -d)
    if [ -n "$dupes" ]; then
        echo "ERROR [$dialect]: duplicate sequence numbers: $dupes"
        fail=1
    fi
    first=$(echo "$sorted" | head -1)
    last=$(echo "$sorted" | tail -1)
    if [ -n "$first" ] && [ -n "$last" ]; then
        expected_seq=$(seq "$((10#$first))" "$((10#$last))")
        actual_seq=$(echo "$sorted" | sed 's/^0*//' | sort -n | uniq)
        missing=$(comm -23 <(echo "$expected_seq") <(echo "$actual_seq" | sort -n) || true)
        if [ -n "$missing" ]; then
            echo "ERROR [$dialect]: sequence gaps at: $(echo $missing | tr '\n' ' ')"
            fail=1
        fi
    fi

    count=$(echo "$sorted" | grep -c . || true)
    echo "OK [$dialect]: $count migration(s), sequence $first..$last"
done

if [ "$fail" -ne 0 ]; then
    echo ""
    echo "FAILED: migration structure check found violations (see above)."
    exit 1
fi
echo "All migration directories structurally valid."
