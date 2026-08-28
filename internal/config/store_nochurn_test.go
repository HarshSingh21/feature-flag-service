package config

import (
	"encoding/json"
	"testing"
	"time"
)

// TestRetriedRejectedPushDoesNotChurnSubscribers pins the fix for a defect found
// by the end-to-end walkthrough in test/demo.
//
// A push whose flags are all quarantined still reaches publication, because
// flag-level quarantine is not an environment-level rejection. Before the fix,
// every retry of a bad layer advanced the generation AND woke every subscriber,
// so a CI job retrying a broken push churned the entire fleet indefinitely while
// the served configuration never changed once.
//
// The contract now separates the two concerns:
//   - generation ALWAYS advances on an accepted build -- it is the operator's
//     audit counter, and "did my push land?" must stay answerable
//   - subscribers are woken ONLY when content a client evaluates actually changed
func TestRetriedRejectedPushDoesNotChurnSubscribers(t *testing.T) {
	s := New(WithEnvironments("prod"))

	var base BaseLayer
	if err := json.Unmarshal([]byte(`{"schema_version":1,"flags":[
	  {"key":"f.int","type":"int","enabled":true,"default_value":50}]}`), &base); err != nil {
		t.Fatal(err)
	}
	if rep := s.Set(&base); rep.Err() != nil {
		t.Fatalf("base rejected: %v", rep.Err())
	}

	ch, cancel := s.Subscribe("prod")
	defer cancel()
	drain(ch)

	before, _ := s.Snapshot("prod")

	// A layer that is invalid at the flag level: the value type contradicts the
	// declared flag type, so the flag is quarantined and last-known-good serves.
	var bad OverlayLayer
	if err := json.Unmarshal([]byte(`{"schema_version":1,"environment":"prod","flags":[
	  {"key":"f.int","default_value":"not-an-integer"}]}`), &bad); err != nil {
		t.Fatal(err)
	}

	for attempt := 1; attempt <= 3; attempt++ {
		rep := s.Set(&bad)
		got, _ := s.Snapshot("prod")

		if rep.Err() == nil {
			t.Fatalf("attempt %d: type-mismatched layer was not rejected", attempt)
		}
		if got.Fingerprint() != before.Fingerprint() {
			t.Fatalf("attempt %d: rejected push mutated served content", attempt)
		}
		// Only the FIRST attempt changes anything a client can observe: it moves
		// the flag into quarantine. Attempts 2 and 3 are true no-ops.
		if wantChanged := attempt == 1; rep.PerEnv["prod"].ContentChanged != wantChanged {
			t.Fatalf("attempt %d: ContentChanged=%v, want %v",
				attempt, rep.PerEnv["prod"].ContentChanged, wantChanged)
		}
	}

	// Exactly one wake-up across three identical rejected pushes.
	if n := drain(ch); n != 1 {
		t.Fatalf("subscribers woken %d times by 3 identical rejected pushes; want 1", n)
	}
}

// drain counts and discards everything currently buffered on the channel.
func drain(ch <-chan *ResolvedSnapshot) int {
	n := 0
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return n
			}
			n++
		case <-deadline:
			return n
		default:
			return n
		}
	}
}
