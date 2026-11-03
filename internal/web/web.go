// Package web holds the frontend. It ships in the binary, so ES needs no Nginx, build
// step, or CORS.
package web

import _ "embed"

// Index is the frontend page.
//
//go:embed index.html
var Index []byte

// Report is the test report page. cmd/report generates it from docs/results.
//
//go:embed report.html
var Report []byte
