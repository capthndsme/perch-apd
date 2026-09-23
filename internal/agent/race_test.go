//go:build race

package agent

// raceEnabled: the race detector's instrumentation allocates, so the
// allocation bounds do not hold under -race.
const raceEnabled = true
