#!/usr/bin/env bash
# Structural validation for migrations/ directories.
#
# Checks (per dialect directory, e.g. migrations/postgres):
#   1. Every NNNNNN_name.up.sql has a matching .down.sql (and vice versa)
#   2. Sequence numbers are unique
#   3. Sequence numbers have no gaps (0-padding is 6 digits by convention),
#      except EXPLICITLY RESERVED ranges (see reserved_gaps below — cross-tree
#      numbering contracts with the commercial overlay's migrations, which are
#      merged into the release chain but absent from the community tree; the
#      dialect behavior_migration_test.go documents the same contract).
#
# Exit code 1 on any violation. POSIX bash — runs in git bash on Windows.
set -euo pipefail

cd "$(dirname "$0")/.."

fail=0

# reserved_gaps: dialect:start:end (inclusive, 1-based sequence numbers).
# Cross-tree numbering contracts with the commercial overlay, whose migrations
# merge into the release chain but are absent here. 2026-11-22: the earlier
# kb_poison/rag_ledger slots (pg 96-98 / mysql 19-21 / sqlite 22-24) were
# filled by community migrations (guardrail key scope, security profiles,
# review services); the current reserved slots sit before the guard-ingest-key
# migration (pg 104 / mysql 27 / sqlite 30) — confirm with the overlay tree
# before renumbering anything into them.
reserved_gaps="
postgres:104:104
mysql:27:27
sqlite:30:30
"

is_reserved() {
    # $1 dialect, $2 number (decimal)
    while IFS= read -r line; do
        [ -z "$line" ] && continue
        d=$(echo "$line" | cut -d: -f1)
        s=$(echo "$line" | cut -d: -f2)
        e=$(echo "$line" | cut -d: -f3)
        if [ "$1" = "$d" ] && [ "$2" -ge "$s" ] && [ "$2" -le "$e" ]; then
            return 0
        fi
    done <<< "$reserved_gaps"
    return 1
}

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
        # Numeric membership check (comm is lexicographic — "10" < "9" — and
        # would mis-order mixed-width numbers, silently missing gaps).
        unreserved_missing=$(echo "$actual_seq" | sort -n | awk -v lo="$((10#$first))" -v hi="$((10#$last))" '
            BEGIN { for (i = lo; i <= hi; i++) want[i] = 1 }
            { delete want[$1] }
            END { for (i = lo; i <= hi; i++) if (i in want) print i }')
        if [ -n "$unreserved_missing" ]; then
            filtered=""
            for m in $unreserved_missing; do
                if ! is_reserved "$dialect" "$m"; then
                    filtered="$filtered $m"
                fi
            done
            if [ -n "$filtered" ]; then
                echo "ERROR [$dialect]: sequence gaps at: $filtered"
                fail=1
            fi
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
