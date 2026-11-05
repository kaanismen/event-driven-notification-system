package notification

import "testing"

func TestCanTransition(t *testing.T) {
	allowed := map[[2]Status]bool{
		{StatusPending, StatusProcessing}: true,
		{StatusPending, StatusCancelled}:  true,
		{StatusProcessing, StatusSent}:    true,
		{StatusProcessing, StatusFailed}:  true,
		{StatusProcessing, StatusPending}: true,
	}
	// Check every pair, so a new status or a typo in the switch cannot slip through.
	for _, from := range Statuses() {
		for _, to := range Statuses() {
			want := allowed[[2]Status{from, to}]
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s) = %t, want %t", from, to, got, want)
			}
		}
	}
}

func TestFinal(t *testing.T) {
	for _, s := range Statuses() {
		hasExit := false
		for _, to := range Statuses() {
			hasExit = hasExit || CanTransition(s, to)
		}
		if s.Final() == hasExit {
			t.Errorf("%s: Final() = %t but has outgoing transitions = %t", s, s.Final(), hasExit)
		}
	}
}
