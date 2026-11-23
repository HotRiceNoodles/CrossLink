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

# is_reserved dialect number: closed-form ranges, no subshells (git-bash on
# Windows spawns processes slowly — a per-gap subshell storm pushed the run
# past CI-visible timeouts once the unified band added ~160 gap numbers).
#
# Reserved ranges:
#  - 2026-11-22 slots before guard-ingest-key (overlay cross-tree contract):
#    pg 104 / mysql 27 / sqlite 30.
#  - 2026-11-23 UNIFIED NUMBERING CUTOVER: from 111 on, all dialects number
#    the same logical migration identically. Each dialect's legacy chain ends
#    at its own tail (pg 105 / mysql 28 / sqlite 31), so the band between the
#    tail and 111 is a one-time transition gap, not drift.
is_reserved() {
    case "$1" in
        postgres)
            [ "$2" -eq 104 ] && return 0
            [ "$2" -ge 106 ] && [ "$2" -le 110 ] && return 0
            ;;
        mysql)
            [ "$2" -eq 27 ] && return 0
            [ "$2" -ge 29 ] && [ "$2" -le 110 ] && return 0
            ;;
        sqlite)
            [ "$2" -eq 30 ] && return 0
            [ "$2" -ge 32 ] && [ "$2" -le 110 ] && return 0
            ;;
    esac
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
