package store

import "testing"

func TestNormalizeAgentMaxTurns(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want int }{
		{0, 0},  // unset → server default
		{-5, 0}, // junk → unset, never negative argv
		{1, 1},
		{80, 80},
		{AgentMaxTurnsLimit, AgentMaxTurnsLimit},
		{AgentMaxTurnsLimit + 1, AgentMaxTurnsLimit}, // a typo must not buy an unbounded run
		{999999, AgentMaxTurnsLimit},
	}
	for _, c := range cases {
		if got := NormalizeAgentMaxTurns(c.in); got != c.want {
			t.Fatalf("NormalizeAgentMaxTurns(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
