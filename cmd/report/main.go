// Command report writes the static test report from the real results in docs/results: a
// Markdown file with SVG charts, and the one page that ES serves at GET /report. It adds
// no value of its own. With -collect-quality, it first runs the tests, the linters, and
// govulncheck, and writes their results to docs/results/quality.json.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// output is one generated file, by its path relative to the repository root.
type output struct {
	path    string
	content string
}

// generate returns every generated file. It uses no clock or random value, so the same
// results always give the same files.
func generate(r results) []output {
	sections := buildSections(r)
	files := []output{
		{"docs/TEST_REPORT.md", renderMarkdown(r, sections)},
		{"internal/web/report.html", renderPage(r, sections)},
	}
	for _, s := range sections {
		if s.Chart != "" {
			files = append(files, output{"docs/report/" + s.Chart + ".svg", s.Canvas.svg(svgStyle)})
		}
	}
	return files
}

func run(root string, collect bool) error {
	dir := filepath.Join(root, "docs", "results")
	if collect {
		if err := collectQuality(root, filepath.Join(dir, "quality.json")); err != nil {
			return err
		}
	}
	r, err := loadResults(dir)
	if err != nil {
		return err
	}
	for _, file := range generate(r) {
		path := filepath.Join(root, file.path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(file.content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	root := flag.String("root", ".", "repository root")
	collect := flag.Bool("collect-quality", false, "run the checks and update docs/results/quality.json first")
	flag.Parse()
	if err := run(*root, *collect); err != nil {
		fmt.Fprintf(os.Stderr, "report: %v\n", err)
		os.Exit(1)
	}
}
