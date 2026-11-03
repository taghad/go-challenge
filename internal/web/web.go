// Package web holds the frontend. It ships in the binary, so ES needs no Nginx, build
// step, or CORS.
package web

import _ "embed"

// Index is the frontend page.
//
//go:embed index.html
var Index []byte
