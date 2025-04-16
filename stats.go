package pool

// Stats represents statistical information about the pool's performance.
type Stats struct {
	// Number of created objects
	CreatedTotal uint
	// Number of objects waited for
	// (i.e. number of "Get"ers that had to wait for a "Put"er)
	WaitedTotal uint
	// Number of destroyed objects
	DestroyedTotal uint

	// Number of objects right now (count == busy + idle)
	CountNow int
	// Number of busy objects right now (busy == count - idle)
	BusyNow int
	// Number of idle objects right now (idle == count - busy)
	IdleNow int
	// Number of objects being waited for right now
	// (i.e. number of "Get"ers currently waiting for a "Put"er)
	WaitingNow int
}
