package estimator

import (
	"errors"
	"fmt"
	"regexp"
)

// MaxBatchSize is the largest batch ValidateBatch accepts. It is a fixed constant
// rather than a setting, because a batch runs as one Redis transaction, which
// blocks Redis while it runs.
const MaxBatchSize = 1000

// maxUserIDBytes limits the size of a user ID. The limit is in bytes because that
// is what Redis stores, whatever the encoding.
const maxUserIDBytes = 128

// segmentPattern is the only rule for a segment name. It is case-sensitive, so
// "Sports" and "sports" are different segments. Names are not trimmed: a name with
// surrounding spaces is rejected instead of being counted under another segment.
var segmentPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Errors for a batch whose size is out of range, which no single event caused.
var (
	ErrEmptyBatch    = errors.New("batch has no events")
	ErrBatchTooLarge = fmt.Errorf("batch has more than %d events", MaxBatchSize)
)

// Event records that a user is in a segment.
type Event struct {
	UserID  string
	Segment string
}

// ValidationError describes the first invalid event of a batch. Reason never
// includes the rejected value, which can be large and comes from the caller.
type ValidationError struct {
	Index  int    // position of the event in the batch, from 0
	Field  string // "user_id" or "segment"
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("event %d: %s %s", e.Index, e.Field, e.Reason)
}

// ValidateBatch returns nil if the whole batch is valid. Otherwise the whole batch
// is rejected, because partial acceptance makes retries harder to reason about.
//
// The size is checked before the events, so an oversized batch costs no per-event
// work and always gets the same error, whatever its events contain.
func ValidateBatch(events []Event) error {
	if len(events) == 0 {
		return ErrEmptyBatch
	}
	if len(events) > MaxBatchSize {
		return ErrBatchTooLarge
	}
	for i, event := range events {
		// A user ID is opaque, because its meaning belongs to USS. Only its size
		// is checked, and it is not trimmed, since " u1" and "u1" may be different
		// users as far as ES can tell.
		if len(event.UserID) == 0 || len(event.UserID) > maxUserIDBytes {
			return &ValidationError{
				Index:  i,
				Field:  "user_id",
				Reason: fmt.Sprintf("must be 1 to %d bytes", maxUserIDBytes),
			}
		}
		if !segmentPattern.MatchString(event.Segment) {
			return &ValidationError{
				Index:  i,
				Field:  "segment",
				Reason: "must match " + segmentPattern.String(),
			}
		}
	}
	return nil
}
