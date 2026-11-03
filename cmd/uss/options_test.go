package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseOptionsDefaults(t *testing.T) {
	o, err := parseOptions(nil)
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if o.url != "http://es:8080" {
		t.Errorf("url = %q, want the compose network address", o.url)
	}
	if o.profile != profileSteady {
		t.Errorf("profile = %q", o.profile)
	}
	if o.seed != 1 {
		t.Errorf("seed = %d", o.seed)
	}
	if o.timeout != 2*time.Second {
		t.Errorf("timeout = %s", o.timeout)
	}
	if o.maxRetries != 3 {
		t.Errorf("maxRetries = %d", o.maxRetries)
	}
	if o.retryJitter <= 0 {
		t.Errorf("retryJitter = %s, want jitter by default", o.retryJitter)
	}
	if o.flags["rate"] != "1000" {
		t.Errorf("flags[rate] = %q, want the flags kept for the report", o.flags["rate"])
	}
}

func TestParseOptionsRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"-url", ""}, "-url"},
		{[]string{"-rate", "0"}, "-rate"},
		{[]string{"-rate", "-1"}, "-rate"},
		{[]string{"-duration", "0s"}, "-duration"},
		{[]string{"-batch", "0"}, "-batch"},
		{[]string{"-users", "0"}, "-users"},
		{[]string{"-segments", "0"}, "-segments"},
		{[]string{"-timeout", "0s"}, "-timeout"},
		{[]string{"-max-retries", "-1"}, "-max-retries"},
		{[]string{"-retry-jitter", "-1s"}, "-retry-jitter"},
		{[]string{"-segment-prefix", "bad:name"}, "-segment-prefix"},
		{[]string{"-segment-prefix", strings.Repeat("a", 64)}, "-segment-prefix"},
		{[]string{"-profile", "uniform"}, "-profile"},
		{[]string{"-profile", "spiky"}, "-profile"},
		{[]string{"-profile", "burst", "-burst-multiplier", "1"}, "-burst-multiplier"},
		{[]string{"-profile", "burst", "-burst-start", "-1s"}, "-burst-start"},
		{[]string{"-profile", "burst", "-burst-duration", "0s"}, "-burst-duration"},
		{[]string{"-profile", "burst", "-duration", "10s", "-burst-start", "8s", "-burst-duration", "3s"}, "burst window"},
		{[]string{"extra"}, "extra"},
		{[]string{"-rate", "fast"}, "rate"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			_, err := parseOptions(tt.args)
			if err == nil {
				t.Fatal("parseOptions returned no error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not name %q", err, tt.want)
			}
		})
	}
}

func TestScheduleSteadyIsEvenlySpaced(t *testing.T) {
	o := options{profile: profileSteady, rate: 1000, batch: 100, duration: time.Second}

	offsets := o.schedule()

	if len(offsets) != 10 {
		t.Fatalf("batches = %d, want 10", len(offsets))
	}
	for i, offset := range offsets {
		want := time.Duration(i) * 100 * time.Millisecond
		if diff := offset - want; diff < -time.Microsecond || diff > time.Microsecond {
			t.Errorf("offset %d = %s, want %s", i, offset, want)
		}
	}
}

func TestScheduleBurstMultipliesRateOnlyInWindow(t *testing.T) {
	o := options{
		profile:         profileBurst,
		rate:            100,
		batch:           100,
		duration:        10 * time.Second,
		burstMultiplier: 10,
		burstStart:      2 * time.Second,
		burstDuration:   time.Second,
	}

	offsets := o.schedule()

	// 9 seconds at 1 batch per second, and 1 second at 10 batches per second.
	if len(offsets) != 19 {
		t.Errorf("batches = %d, want 19", len(offsets))
	}
	var inWindow int
	for _, offset := range offsets {
		if offset >= o.burstStart && offset < o.burstStart+o.burstDuration {
			inWindow++
		}
	}
	if inWindow != 10 {
		t.Errorf("batches in the window = %d, want 10", inWindow)
	}
}

func TestSteadyIgnoresBurstSettings(t *testing.T) {
	o := options{profile: profileSteady, rate: 100, burstMultiplier: 10, burstDuration: time.Hour}
	if got := o.rateAt(time.Second); got != 100 {
		t.Errorf("rateAt = %f, want 100", got)
	}
}
