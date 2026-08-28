//go:build !race

package load

// raceEnabled reports whether this binary was built with -race. See
// race_on_test.go for why it gates the throughput scenarios.
const raceEnabled = false
