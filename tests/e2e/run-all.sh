#!/usr/bin/env bash
#
# Run every numbered scenario in lexical order. Each test is a
# self-contained docker container that builds, runs, asserts, tears
# down. Failure of one test does NOT abort the suite — we keep going
# so the operator gets the full failure list per run, like a normal
# unit-test framework.
#
# Exit status: 0 if every test passed, 1 if any failed, 77 if any
# was skipped (autotools convention — propagated through so CI can
# distinguish "feature absent" from "regression").

set -u

cd "$(dirname "$0")"

declare -i passed=0 failed=0 skipped=0
declare -a failed_tests=()
declare -a skipped_tests=()

for script in $(ls -1 [0-9][0-9]-*.sh 2>/dev/null | sort); do
    echo
    echo "==================== $script ===================="
    if bash "./$script"; then
        passed=$((passed + 1))
    else
        rc=$?
        if [ "$rc" -eq 77 ]; then
            skipped=$((skipped + 1))
            skipped_tests+=("$script")
        else
            failed=$((failed + 1))
            failed_tests+=("$script")
        fi
    fi
done

echo
echo "================ SUMMARY ================"
echo "passed:  $passed"
echo "failed:  $failed${failed_tests:+ ( ${failed_tests[*]} )}"
echo "skipped: $skipped${skipped_tests:+ ( ${skipped_tests[*]} )}"

if [ "$failed" -gt 0 ]; then
    exit 1
fi
if [ "$skipped" -gt 0 ] && [ "$passed" -eq 0 ]; then
    exit 77
fi
exit 0
