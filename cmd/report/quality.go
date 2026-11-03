package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	testCommand        = "go test -p 1 -race -count=1 -cover -json ./..."
	govulncheckVersion = "v1.9.0"
)

var coverageLine = regexp.MustCompile(`coverage: ([0-9.]+)% of statements`)

// parseTestEvents reads the stream of `go test -json`. It counts each test and subtest once.
func parseTestEvents(stream io.Reader) ([]packageResult, error) {
	packages := map[string]*packageResult{}
	decoder := json.NewDecoder(stream)
	for {
		var event struct {
			Action, Package, Test, Output string
		}
		if err := decoder.Decode(&event); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("parse go test output: %w", err)
		}
		if event.Package == "" {
			continue
		}
		p, ok := packages[event.Package]
		if !ok {
			p = &packageResult{Name: strings.TrimPrefix(event.Package, "github.com/taghad/go-challenge/")}
			packages[event.Package] = p
		}
		if match := coverageLine.FindStringSubmatch(event.Output); match != nil {
			if coverage, err := strconv.ParseFloat(match[1], 64); err == nil {
				p.CoveragePercent = &coverage
			}
		}
		if event.Test == "" {
			continue
		}
		switch event.Action {
		case "pass":
			p.Passed++
		case "fail":
			p.Failed++
		case "skip":
			p.Skipped++
		}
	}
	result := make([]packageResult, 0, len(packages))
	for _, p := range packages {
		result = append(result, *p)
	}
	slices.SortFunc(result, func(a, b packageResult) int { return strings.Compare(a.Name, b.Name) })
	return result, nil
}

// parseVulnFindings reads the stream of `govulncheck -format json`. A vulnerability has the
// level of its deepest finding: symbol if the code reaches it, then package, then module.
func parseVulnFindings(stream io.Reader) ([]vulnFinding, error) {
	rank := map[string]int{"module": 0, "package": 1, "symbol": 2}
	levels := map[string]string{}
	decoder := json.NewDecoder(stream)
	for {
		var message struct {
			Finding *struct {
				OSV   string `json:"osv"`
				Trace []struct {
					Package  string `json:"package"`
					Function string `json:"function"`
				} `json:"trace"`
			} `json:"finding"`
		}
		if err := decoder.Decode(&message); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("parse govulncheck output: %w", err)
		}
		if message.Finding == nil || len(message.Finding.Trace) == 0 {
			continue
		}
		level := "module"
		switch frame := message.Finding.Trace[0]; {
		case frame.Function != "":
			level = "symbol"
		case frame.Package != "":
			level = "package"
		}
		if current, ok := levels[message.Finding.OSV]; !ok || rank[level] > rank[current] {
			levels[message.Finding.OSV] = level
		}
	}
	findings := []vulnFinding{}
	for id, level := range levels {
		findings = append(findings, vulnFinding{ID: id, Level: level})
	}
	slices.SortFunc(findings, func(a, b vulnFinding) int { return strings.Compare(a.ID, b.ID) })
	return findings, nil
}

// collectQuality runs the checks in root and writes what they report. A check that fails
// is recorded as failed, and it does not stop the others.
func collectQuality(root, path string) error {
	command := func(name string, args ...string) *exec.Cmd {
		cmd := exec.Command(name, args...)
		cmd.Dir = root
		return cmd
	}
	var tests bytes.Buffer
	testRun := command("go", strings.Fields(testCommand)[1:]...)
	testRun.Stdout = &tests
	testErr := testRun.Run()
	packages, err := parseTestEvents(&tests)
	if err != nil {
		return err
	}
	vet := command("go", "vet", "./...").Run()
	gofmt, gofmtErr := command("gofmt", "-l", ".").Output()
	var vulns bytes.Buffer
	vulnRun := command("go", "run", "golang.org/x/vuln/cmd/govulncheck@"+govulncheckVersion, "-format", "json", "./...")
	vulnRun.Stdout = &vulns
	if err := vulnRun.Run(); err != nil {
		return fmt.Errorf("govulncheck: %w", err)
	}
	findings, err := parseVulnFindings(bufio.NewReader(&vulns))
	if err != nil {
		return err
	}
	result := qualityResult{
		Source:             "go run ./cmd/report -collect-quality, in the compose test service",
		GeneratedAt:        time.Now().UTC().Format(time.RFC3339),
		Go:                 runtime.Version(),
		TestCommand:        testCommand,
		TestsPassed:        testErr == nil,
		RaceEnabled:        strings.Contains(testCommand, "-race"),
		Packages:           packages,
		VetClean:           vet == nil,
		GofmtClean:         gofmtErr == nil && len(bytes.TrimSpace(gofmt)) == 0,
		GovulncheckVersion: govulncheckVersion,
		Findings:           findings,
	}
	content, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(content, '\n'), 0o644)
}
