package estimator

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func validEvents(n int) []Event {
	return slices.Repeat([]Event{{UserID: "u1", Segment: "sports"}}, n)
}

func TestValidateBatchSize(t *testing.T) {
	tests := []struct {
		name   string
		events []Event
		want   error
	}{
		{"nil", nil, ErrEmptyBatch},
		{"empty", []Event{}, ErrEmptyBatch},
		{"one event", validEvents(1), nil},
		{"at the limit", validEvents(MaxBatchSize), nil},
		{"over the limit", validEvents(MaxBatchSize + 1), ErrBatchTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidateBatch(tt.events); !errors.Is(got, tt.want) {
				t.Errorf("ValidateBatch = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMaxBatchSizeIsFixed(t *testing.T) {
	if MaxBatchSize != 1000 {
		t.Errorf("MaxBatchSize = %d, want 1000", MaxBatchSize)
	}
}

func TestValidateBatchChecksSizeBeforeEvents(t *testing.T) {
	events := slices.Repeat([]Event{{UserID: "", Segment: "not valid"}}, MaxBatchSize+1)

	if got := ValidateBatch(events); !errors.Is(got, ErrBatchTooLarge) {
		t.Errorf("ValidateBatch = %v, want %v", got, ErrBatchTooLarge)
	}
}

func TestValidateBatchUserID(t *testing.T) {
	tests := []struct {
		name   string
		userID string
		valid  bool
	}{
		{"empty", "", false},
		{"one byte", "u", true},
		{"at the limit", strings.Repeat("u", 128), true},
		{"over the limit", strings.Repeat("u", 129), false},
		{"64 two-byte runes are 128 bytes", strings.Repeat("é", 64), true},
		{"65 two-byte runes are 130 bytes", strings.Repeat("é", 65), false},
		{"surrounding spaces are kept", " u1 ", true},
		{"only spaces", "   ", true},
		{"newline", "u1\n", true},
		{"not valid UTF-8", "\xff\xfe", true},
		{"any punctuation", "user:1/a@b.c", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateBatch([]Event{{UserID: tt.userID, Segment: "sports"}})
			checkEvent(t, err, tt.valid, "user_id")
		})
	}
}

// segmentCases is shared so that reads and writes are held to the same rule.
var segmentCases = []struct {
	name    string
	segment string
	valid   bool
}{
	{"empty", "", false},
	{"one character", "s", true},
	{"at the limit", strings.Repeat("s", 64), true},
	{"over the limit", strings.Repeat("s", 65), false},
	{"lower case", "sports", true},
	{"upper case", "SPORTS", true},
	{"digits, underscore, hyphen", "a0_b-1", true},
	{"leading space", " sports", false},
	{"trailing space", "sports ", false},
	{"trailing newline", "sports\n", false},
	{"leading newline", "\nsports", false},
	{"inner space", "spo rts", false},
	{"hash tag braces", "{sports}", false},
	{"key separator", "sports:1", false},
	{"non-ASCII letter", "spörts", false},
}

func TestValidateBatchSegment(t *testing.T) {
	for _, tt := range segmentCases {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateBatch([]Event{{UserID: "u1", Segment: tt.segment}})
			checkEvent(t, err, tt.valid, "segment")
		})
	}
}

func TestValidateSegment(t *testing.T) {
	for _, tt := range segmentCases {
		t.Run(tt.name, func(t *testing.T) {
			var want error
			if !tt.valid {
				want = ErrInvalidSegment
			}
			if got := ValidateSegment(tt.segment); got != want {
				t.Errorf("ValidateSegment = %v, want %v", got, want)
			}
		})
	}
}

// checkEvent checks the result of validating a batch whose event 0 is under test.
func checkEvent(t *testing.T, err error, valid bool, field string) {
	t.Helper()
	if valid {
		if err != nil {
			t.Errorf("got error %v, want nil", err)
		}
		return
	}
	var got *ValidationError
	if !errors.As(err, &got) {
		t.Fatalf("got error %v, want a ValidationError", err)
	}
	if got.Index != 0 || got.Field != field {
		t.Errorf("got index %d field %q, want index 0 field %q", got.Index, got.Field, field)
	}
	if got.Reason == "" {
		t.Error("Reason is empty")
	}
}

func TestValidateBatchDoesNotChangeEvents(t *testing.T) {
	events := []Event{{UserID: " u1 ", Segment: "Sports"}}
	want := slices.Clone(events)

	if err := ValidateBatch(events); err != nil {
		t.Fatalf("ValidateBatch: %v", err)
	}
	if !slices.Equal(events, want) {
		t.Errorf("events = %+v, want %+v", events, want)
	}
}

func TestValidateBatchReturnsFirstInvalidEvent(t *testing.T) {
	events := validEvents(5)
	events[2].Segment = "not valid"
	events[3].UserID = ""

	var got *ValidationError
	if !errors.As(ValidateBatch(events), &got) {
		t.Fatal("want a ValidationError")
	}
	if got.Index != 2 || got.Field != "segment" {
		t.Errorf("got index %d field %q, want index 2 field %q", got.Index, got.Field, "segment")
	}
}

func TestValidateBatchFindsInvalidEventAtTheEnd(t *testing.T) {
	events := validEvents(MaxBatchSize)
	events[MaxBatchSize-1].UserID = ""

	var got *ValidationError
	if !errors.As(ValidateBatch(events), &got) {
		t.Fatal("want a ValidationError")
	}
	if got.Index != MaxBatchSize-1 {
		t.Errorf("Index = %d, want %d", got.Index, MaxBatchSize-1)
	}
}

func TestValidationErrorMessage(t *testing.T) {
	err := ValidateBatch([]Event{{UserID: "u1", Segment: "sports"}, {UserID: "", Segment: "sports"}})
	want := "event 1: user_id must be 1 to 128 bytes"
	if err == nil || err.Error() != want {
		t.Errorf("Error = %v, want %q", err, want)
	}
}

func TestValidationErrorOmitsRejectedValue(t *testing.T) {
	long := strings.Repeat("x", 200)
	for _, event := range []Event{
		{UserID: long, Segment: "sports"},
		{UserID: "u1", Segment: long},
	} {
		if err := ValidateBatch([]Event{event}); err == nil || strings.Contains(err.Error(), long) {
			t.Errorf("Error = %v, want one without the rejected value", err)
		}
	}
}
