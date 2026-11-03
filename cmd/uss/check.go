package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// estimateMargin is 3 standard errors of about 0.81% [E]. It is an owner decision and an
// assumption, fixed in docs/DESIGN.md.
const estimateMargin = 0.0243

// estimateCheck compares each segment's estimate with its distinct users.
type estimateCheck struct {
	Segments   int            `json:"segments"`
	InRange    int            `json:"in_range"`
	OutOfRange int            `json:"out_of_range"`
	Failed     int            `json:"failed"`
	Results    []segmentCheck `json:"results"`
}

type segmentCheck struct {
	Segment      string  `json:"segment"`
	Acknowledged int64   `json:"acknowledged"`
	Attempted    int64   `json:"attempted"`
	Estimate     uint64  `json:"estimate"`
	Low          float64 `json:"low"`
	High         float64 `json:"high"`
	InRange      bool    `json:"in_range"`
	Error        string  `json:"error,omitempty"`
}

// estimateRange returns the range an estimate must lie in. A timed-out event may have
// been written, so the true number of users lies between the acknowledged and the
// attempted users, and HyperLogLog error adds the margin on each side.
func estimateRange(counts userCounts) (low, high float64) {
	return float64(counts.Acknowledged) * (1 - estimateMargin), float64(counts.Attempted) * (1 + estimateMargin)
}

// checkEstimates asks ES for every segment, one at a time, after a delay that is longer
// than the cache TTL. A segment that ES does not answer counts as failed, and not as out
// of range, because it says nothing about the estimate.
func checkEstimates(ctx context.Context, client *http.Client, o options, segments []segmentCounts) estimateCheck {
	select {
	case <-ctx.Done():
	case <-time.After(o.checkDelay):
	}
	check := estimateCheck{Segments: len(segments)}
	for _, segment := range segments {
		low, high := estimateRange(segment.userCounts)
		result := segmentCheck{
			Segment:      segment.Segment,
			Acknowledged: segment.Acknowledged,
			Attempted:    segment.Attempted,
			Low:          low,
			High:         high,
		}
		estimate, err := fetchEstimate(client, o, segment.Segment)
		switch {
		case err != nil:
			result.Error = err.Error()
			check.Failed++
		case float64(estimate) >= low && float64(estimate) <= high:
			result.Estimate, result.InRange = estimate, true
			check.InRange++
		default:
			result.Estimate = estimate
			check.OutOfRange++
		}
		check.Results = append(check.Results, result)
	}
	return check
}

// fetchEstimate retries a 503 as a client of ES should, up to the retry limit.
func fetchEstimate(client *http.Client, o options, segment string) (uint64, error) {
	url := strings.TrimSuffix(o.url, "/") + "/v1/segments/" + segment + "/estimate"
	var lastErr error
	for range o.maxRetries + 1 {
		estimate, wait, err := getEstimate(client, url, o.timeout)
		if err == nil {
			return estimate, nil
		}
		lastErr = err
		if wait > 0 {
			time.Sleep(wait)
		}
	}
	return 0, lastErr
}

// getEstimate makes one attempt. A non-zero wait means the attempt can be repeated.
func getEstimate(client *http.Client, url string, timeout time.Duration) (uint64, time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, err
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, defaultRetryWait, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusServiceUnavailable {
		return 0, parseRetryAfter(response.Header.Get("Retry-After")), fmt.Errorf("status %d", response.StatusCode)
	}
	if response.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("status %d", response.StatusCode)
	}
	var body struct {
		Estimate uint64 `json:"estimate"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return 0, 0, err
	}
	return body.Estimate, 0, nil
}
