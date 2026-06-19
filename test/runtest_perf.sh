#!/bin/sh -eux

# Performance regression test: a large (+1y) offset must NOT make the target
# crawl. Pre-fix the fake clock_gettime normalize loop ran ~31.5M times per call
# (~31ms); post-fix it is a single syscall (~µs). See fake_clock_gettime.c and
# cmd/watchmaker.go (offset split that keeps |nsec_delta| < 1e9).

if [ "${GITHUB_RUN_ID}" -gt 0 ]; then
    _SUDO="sudo"
else
    _SUDO=
fi

TESTROOT="$1"
CEILING_NS=100000  # post-fix ~1e3 ns/call; pre-fix ~3e7 ns/call
OUTPUT=$(mktemp "/tmp/test-clock_perf.XXXXXX")

cleanup() {
    rm -f "${OUTPUT}"
    kill "${pid}" 2>/dev/null || true
}

trap "cleanup" EXIT

_GOARCH=$(go env GOARCH)

if [ ! -x "${TESTROOT}/test_clock_perf" ]; then
    echo "${TESTROOT}/test_clock_perf not found" >&2
    exit 1
fi

# timeout is a second fail signal: pre-fix the batch takes hours, so the victim
# gets killed and `wait` reports failure even before the threshold check.
timeout 60 "${TESTROOT}/test_clock_perf" >"${OUTPUT}" 2>&1 &

pid=$!

sleep 1

${_SUDO} "${TESTROOT}/../bin/watchmaker_linux_${_GOARCH}" --faketime '+1y' --pid "$pid"

wait "$pid" || { echo "victim timed out -> +1y hung the process (regression)" >&2; exit 1; }

cat "${OUTPUT}"

per_call=$(awk -F= '/per_call_ns/{print int($2)}' "${OUTPUT}")
[ -n "${per_call}" ] || { echo "no measurement produced" >&2; exit 1; }

if [ "${per_call}" -gt "${CEILING_NS}" ]; then
    echo "FAIL: ${per_call} ns/call > ${CEILING_NS} ns -- +1y offset is slow again" >&2
    exit 1
fi

echo "PASS: ${per_call} ns/call <= ${CEILING_NS} ns"
