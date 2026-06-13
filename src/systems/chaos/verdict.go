package main

import "strings"

// isTerminal reports whether a status is a final state that should never be
// overwritten by a later (possibly duplicate) event.
func isTerminal(status string) bool {
	switch status {
	case StatusPass, StatusFail, StatusError, StatusStopped:
		return true
	default:
		return false
	}
}

// verdictToStatus maps a litmuschaos ChaosResult verdict (and phase, read
// defensively) to one of the experiment status strings the rest of the platform
// understands. The mapping is intentionally permissive: anything that is not a
// clean Pass while completed is treated as a failure, and an Awaited/empty
// verdict on a still-running result keeps the experiment running.
//
//	verdict  Pass                       -> Pass
//	verdict  Fail                       -> Fail
//	verdict  Error / Stopped            -> Error
//	verdict  Awaited / "" (Completed)   -> Error   (completed with no verdict is a failure)
//	verdict  Awaited / "" (not done)    -> running
func verdictToStatus(verdict, phase string) string {
	switch strings.ToLower(strings.TrimSpace(verdict)) {
	case "pass":
		return StatusPass
	case "fail":
		return StatusFail
	case "error", "stopped":
		return StatusError
	}
	// No decisive verdict yet. If the experiment phase says it has finished, an
	// absent verdict is a failure; otherwise it is still in flight.
	if isCompletedPhase(phase) {
		return StatusError
	}
	return StatusRunning
}

func isCompletedPhase(phase string) bool {
	switch strings.ToLower(strings.TrimSpace(phase)) {
	case "completed", "complete", "stopped":
		return true
	default:
		return false
	}
}
