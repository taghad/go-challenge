// Command uss simulates the User Segmentation Service. It sends an open-loop schedule
// of event batches to ES, retries as USS is assumed to, and prints a JSON report. With
// -ramp, it doubles the rate at each step until the saturation rule stops it.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	o, err := parseOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid options:\n%v\n", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	client := newClient()
	var result any
	if o.ramp {
		result = runRamp(ctx, o, client)
	} else {
		run := newSimulator(o, client).run(ctx)
		if o.checkEstimates {
			check := checkEstimates(ctx, client, o, run.Segments)
			run.EstimateCheck = &check
		}
		result = run
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		fmt.Fprintf(os.Stderr, "write report: %v\n", err)
		os.Exit(1)
	}
}
