//go:build race

package load

// raceEnabled reports whether this binary was built with -race.
//
// Throughput and latency numbers from a race-instrumented binary are not
// numbers about this design; they are numbers about ThreadSanitizer's shadow
// memory. Every scenario that reports a rate or a percentile refuses to run
// under -race rather than printing something that looks like a measurement.
const raceEnabled = true
