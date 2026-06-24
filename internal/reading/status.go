package reading

// Status is the processing lifecycle state for a reading.
type Status string

const (
	// Pending means the reading has been accepted and is waiting for processing.
	Pending Status = "pending"
	// Running means a worker is currently processing the reading.
	Running Status = "running"
	// Ready means processing completed successfully.
	Ready Status = "ready"
	// Failed means processing failed but the reading may be reprocessed.
	Failed Status = "failed"
)

var allowedTransitions = map[Status]map[Status]bool{
	Pending: {
		Running: true,
		Failed:  true,
	},
	Running: {
		Ready:   true,
		Failed:  true,
		Pending: true,
	},
	Ready: {
		Pending: true,
	},
	Failed: {
		Pending: true,
	},
}

// CanTransition reports whether a reading may move directly from one status to another.
func CanTransition(from, to Status) bool {
	return allowedTransitions[from][to]
}

// IsTerminal reports whether a status represents a completed processing attempt.
func (s Status) IsTerminal() bool {
	return s == Ready || s == Failed
}
