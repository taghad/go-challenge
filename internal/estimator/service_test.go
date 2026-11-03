package estimator

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

type addCall struct {
	ctx    context.Context
	bucket Bucket
	events []Event
}

type countCall struct {
	ctx     context.Context
	segment string
	buckets []Bucket
}

// fakeStore records its calls and returns the configured count and error.
type fakeStore struct {
	addCalls   []addCall
	countCalls []countCall
	count      uint64
	err        error
}

func (f *fakeStore) Add(ctx context.Context, bucket Bucket, events []Event) error {
	f.addCalls = append(f.addCalls, addCall{ctx, bucket, events})
	return f.err
}

func (f *fakeStore) Count(ctx context.Context, segment string, buckets []Bucket) (uint64, error) {
	f.countCalls = append(f.countCalls, countCall{ctx, segment, buckets})
	return f.count, f.err
}

func (f *fakeStore) Ping(context.Context) error {
	return f.err
}

// fakeClock moves forward on every read, so that a second read in one operation
// would land in the next bucket and be seen by the tests.
type fakeClock struct {
	next  time.Time
	reads int
}

func (c *fakeClock) now() time.Time {
	c.reads++
	current := c.next
	c.next = c.next.Add(time.Second)
	return current
}

// justBeforeBoundary is one second before the 6h boundary at the boundary constant.
var justBeforeBoundary = time.Unix(boundary-1, 0)

func newTestService(t *testing.T, store Store) (*Service, *fakeClock, Window) {
	t.Helper()
	window := mustWindow(t, 6*time.Hour)
	clock := &fakeClock{next: justBeforeBoundary}
	return NewService(store, window, clock.now, testTTL, testMaxInflight), clock, window
}

type ctxKey struct{}

func TestAddWritesBatchToOneBucket(t *testing.T) {
	store := &fakeStore{}
	service, clock, window := newTestService(t, store)
	events := []Event{
		{UserID: "u1", Segment: "sports"},
		{UserID: "u2", Segment: "news"},
		{UserID: "u3", Segment: "sports"},
	}

	if err := service.Add(context.Background(), events); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if clock.reads != 1 {
		t.Errorf("clock reads = %d, want 1", clock.reads)
	}
	if len(store.addCalls) != 1 {
		t.Fatalf("store Add calls = %d, want 1", len(store.addCalls))
	}
	call := store.addCalls[0]
	if want := window.Current(justBeforeBoundary); call.bucket != want {
		t.Errorf("bucket = %+v, want %+v", call.bucket, want)
	}
	// The batch arrives whole and in order: grouping belongs to the store.
	if !slices.Equal(call.events, events) {
		t.Errorf("events = %+v, want %+v", call.events, events)
	}
}

func TestAddRejectsInvalidBatchBeforeClockAndStore(t *testing.T) {
	tests := []struct {
		name   string
		events []Event
		want   error
	}{
		{"empty", nil, ErrEmptyBatch},
		{"too large", validEvents(MaxBatchSize + 1), ErrBatchTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeStore{}
			service, clock, _ := newTestService(t, store)

			if got := service.Add(context.Background(), tt.events); got != tt.want {
				t.Errorf("Add = %v, want %v", got, tt.want)
			}
			if clock.reads != 0 || len(store.addCalls) != 0 {
				t.Errorf("clock reads = %d, store calls = %d, want 0 and 0", clock.reads, len(store.addCalls))
			}
		})
	}
}

func TestAddReturnsValidationError(t *testing.T) {
	store := &fakeStore{}
	service, clock, _ := newTestService(t, store)
	events := validEvents(3)
	events[1].UserID = ""

	var got *ValidationError
	if !errors.As(service.Add(context.Background(), events), &got) {
		t.Fatal("want a ValidationError")
	}
	if got.Index != 1 || got.Field != "user_id" {
		t.Errorf("got index %d field %q, want index 1 field %q", got.Index, got.Field, "user_id")
	}
	if clock.reads != 0 || len(store.addCalls) != 0 {
		t.Errorf("clock reads = %d, store calls = %d, want 0 and 0", clock.reads, len(store.addCalls))
	}
}

func TestAddWrapsStoreError(t *testing.T) {
	storeErr := errors.New("store down")
	service, _, _ := newTestService(t, &fakeStore{err: storeErr})

	err := service.Add(context.Background(), validEvents(1))
	if !errors.Is(err, storeErr) {
		t.Errorf("Add = %v, want it to wrap %v", err, storeErr)
	}
	if err == storeErr {
		t.Error("store error is returned without context")
	}
}

func TestAddPassesContextToStore(t *testing.T) {
	store := &fakeStore{}
	service, _, _ := newTestService(t, store)
	ctx := context.WithValue(context.Background(), ctxKey{}, "value")

	if err := service.Add(ctx, validEvents(1)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if store.addCalls[0].ctx.Value(ctxKey{}) != "value" {
		t.Error("store did not receive the caller's context")
	}
}

func TestEstimateWindowAndTimeAgree(t *testing.T) {
	store := &fakeStore{count: 1 << 40}
	service, clock, window := newTestService(t, store)

	got, err := service.Estimate(context.Background(), "sports")
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}

	if clock.reads != 1 {
		t.Errorf("clock reads = %d, want 1", clock.reads)
	}
	if got.Value != 1<<40 {
		t.Errorf("Value = %d, want %d", got.Value, uint64(1<<40))
	}
	if !got.AsOf.Equal(justBeforeBoundary) {
		t.Errorf("AsOf = %s, want %s", got.AsOf, justBeforeBoundary)
	}
	if len(store.countCalls) != 1 {
		t.Fatalf("store Count calls = %d, want 1", len(store.countCalls))
	}
	call := store.countCalls[0]
	if call.segment != "sports" {
		t.Errorf("segment = %q, want %q", call.segment, "sports")
	}
	if want := window.Buckets(justBeforeBoundary); !slices.Equal(call.buckets, want) {
		t.Errorf("buckets start at %d, want %d", call.buckets[0].Start, want[0].Start)
	}
}

func TestEstimateAsOfIsUTC(t *testing.T) {
	zone := time.FixedZone("UTC+3:30", 3*3600+1800)
	clock := &fakeClock{next: justBeforeBoundary.In(zone)}
	service := NewService(&fakeStore{}, mustWindow(t, 6*time.Hour), clock.now, testTTL, testMaxInflight)

	got, err := service.Estimate(context.Background(), "sports")
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if got.AsOf.Location() != time.UTC {
		t.Errorf("AsOf location = %s, want UTC", got.AsOf.Location())
	}
	if !got.AsOf.Equal(justBeforeBoundary) {
		t.Errorf("AsOf = %s, want %s", got.AsOf, justBeforeBoundary)
	}
}

func TestEstimateUnknownSegmentReturnsZero(t *testing.T) {
	service, _, _ := newTestService(t, &fakeStore{count: 0})

	got, err := service.Estimate(context.Background(), "unknown")
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if got.Value != 0 {
		t.Errorf("Value = %d, want 0", got.Value)
	}
}

func TestEstimateRejectsInvalidSegmentBeforeClockAndStore(t *testing.T) {
	store := &fakeStore{}
	service, clock, _ := newTestService(t, store)

	got, err := service.Estimate(context.Background(), "not valid")
	if err != ErrInvalidSegment {
		t.Errorf("Estimate error = %v, want %v", err, ErrInvalidSegment)
	}
	if got != (Estimate{}) {
		t.Errorf("Estimate = %+v, want the zero value", got)
	}
	if clock.reads != 0 || len(store.countCalls) != 0 {
		t.Errorf("clock reads = %d, store calls = %d, want 0 and 0", clock.reads, len(store.countCalls))
	}
}

func TestEstimateWrapsStoreError(t *testing.T) {
	storeErr := errors.New("store down")
	service, _, _ := newTestService(t, &fakeStore{count: 5, err: storeErr})

	got, err := service.Estimate(context.Background(), "sports")
	if !errors.Is(err, storeErr) {
		t.Errorf("Estimate error = %v, want it to wrap %v", err, storeErr)
	}
	if err == storeErr {
		t.Error("store error is returned without context")
	}
	if got != (Estimate{}) {
		t.Errorf("Estimate = %+v, want the zero value", got)
	}
}

func TestEstimatePassesContextToStore(t *testing.T) {
	store := &fakeStore{}
	service, _, _ := newTestService(t, store)
	ctx := context.WithValue(context.Background(), ctxKey{}, "value")

	if _, err := service.Estimate(ctx, "sports"); err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if store.countCalls[0].ctx.Value(ctxKey{}) != "value" {
		t.Error("store did not receive the caller's context")
	}
}
