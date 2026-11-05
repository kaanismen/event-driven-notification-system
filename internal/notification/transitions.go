package notification

// CanTransition reports whether the lifecycle allows moving from one status to another
// (see the state diagram in docs/architecture.md). The store enforces the same rules in
// SQL with guarded UPDATEs such as "WHERE status = 'pending'"; this function is the
// readable specification and lets tests check the two agree.
func CanTransition(from, to Status) bool {
	switch from {
	case StatusPending:
		// Claimed by a worker, or cancelled by a client.
		return to == StatusProcessing || to == StatusCancelled
	case StatusProcessing:
		// Delivered; given up (permanent error or no attempts left); or back to the queue
		// after a transient error, an expired lease or a shutdown release.
		return to == StatusSent || to == StatusFailed || to == StatusPending
	}
	// sent, failed and cancelled are final.
	return false
}

// Final reports whether no further transition is possible.
func (s Status) Final() bool {
	return s == StatusSent || s == StatusFailed || s == StatusCancelled
}
