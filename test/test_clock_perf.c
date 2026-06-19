#include <stdio.h>
#include <time.h>

// Performance regression guard for large time offsets.
//
// The injected fake_clock_gettime.c normalizes the nanosecond delta with a loop
// that subtracts 1e9 one iteration at a time. If the whole offset is handed over
// as nanoseconds, that loop runs (offset_in_seconds) times on EVERY call --
// ~31.5M iterations for a +1y skew (~31ms/call), making the target crawl.
// cmd/watchmaker.go now splits the offset so |nsec_delta| < 1e9, bounding the
// loop to one iteration. This program measures per-call cost after a +1y skew.
//
// We time the batch with CLOCK_MONOTONIC, which is NOT in the default injected
// clock-id mask, so it stays a cheap, un-skewed reference timer.

#define DETECT_JUMP_SEC (200L * 24 * 3600)  // >200 days forward = injection landed
#define BATCH 200000

static double mono_ns(void) {
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return (double)t.tv_sec * 1e9 + t.tv_nsec;
}

int main(void) {
    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    time_t base = ts.tv_sec;

    // wait until the injector shifts CLOCK_REALTIME a long way forward
    for (;;) {
        clock_gettime(CLOCK_REALTIME, &ts);
        if (ts.tv_sec - base > DETECT_JUMP_SEC) {
            break;
        }
        struct timespec nap = {0, 50 * 1000 * 1000};  // 50ms
        nanosleep(&nap, NULL);
    }

    double t0 = mono_ns();
    volatile long sink = 0;
    for (int i = 0; i < BATCH; i++) {
        clock_gettime(CLOCK_REALTIME, &ts);
        sink += ts.tv_nsec;
    }
    double t1 = mono_ns();

    printf("per_call_ns=%.1f\n", (t1 - t0) / BATCH);
    return 0;
}
