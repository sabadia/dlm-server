package domain

import "time"

// RefreshOutcome is the result of one link-refresh step.
type RefreshOutcome string

// Refresh outcomes.
const (
	// RefreshSuccess: a new link was validated and applied.
	RefreshSuccess RefreshOutcome = "success"
	// RefreshFailed: the strategy could not produce a working link.
	RefreshFailed RefreshOutcome = "failed"
	// RefreshMismatch: the new link serves different content than the one
	// downloaded so far (fingerprint mismatch) and the policy did not
	// allow using it.
	RefreshMismatch RefreshOutcome = "mismatch"
	// RefreshRestarted: the new link serves different content and the
	// policy was to restart: partial data was discarded.
	RefreshRestarted RefreshOutcome = "restarted"
	// RefreshWaiting: the task waits for an external event (an extension
	// capture or a manual link update).
	RefreshWaiting RefreshOutcome = "waiting"
	// RefreshTimeout: the wait ended without a new link.
	RefreshTimeout RefreshOutcome = "timeout"
	// RefreshSkipped: the strategy does not apply or is not available.
	RefreshSkipped RefreshOutcome = "skipped"
)

// Valid reports whether o is a known outcome.
func (o RefreshOutcome) Valid() bool {
	switch o {
	case RefreshSuccess, RefreshFailed, RefreshMismatch, RefreshRestarted, RefreshWaiting, RefreshTimeout, RefreshSkipped:
		return true
	default:
		return false
	}
}

// What started a refresh.
const (
	// RefreshCauseExpired: the download failed with an expired link.
	RefreshCauseExpired = "expired"
	// RefreshCauseManual: the user asked for a refresh.
	RefreshCauseManual = "manual"
	// RefreshCauseCapture: a browser capture matched the waiting task.
	RefreshCauseCapture = "capture"
	// RefreshCauseSource: the user set a new link.
	RefreshCauseSource = "source"
	// RefreshCauseTimeout: the wait for a capture timed out and the chain
	// continued.
	RefreshCauseTimeout = "timeout"
)

// RefreshAttempt is one entry of a task's refresh history: a strategy that
// ran (or was skipped), or a wait that started or ended.
type RefreshAttempt struct {
	ID     string `json:"id"`
	TaskID string `json:"taskId"`
	// Strategy is the settings.RefreshStrategy that acted.
	Strategy string `json:"strategy"`
	// Cause says what started the refresh (RefreshCause*).
	Cause   string         `json:"cause"`
	Outcome RefreshOutcome `json:"outcome"`
	// Error explains a failure, mismatch or skip.
	Error string `json:"error,omitempty"`
	// OldURL and NewURL are the links before and after (NewURL only when a
	// link was produced).
	OldURL string `json:"oldUrl,omitempty"`
	NewURL string `json:"newUrl,omitempty"`
	// Downloaded is the task's downloaded bytes at the time; attempts that
	// did not lead to progress count towards refresh.maxAttempts.
	Downloaded int64 `json:"downloaded"`
	// Deadline is when a waiting task gives up (RefreshWaiting only; absent
	// for waits without a timeout).
	Deadline  *time.Time `json:"deadline,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}
