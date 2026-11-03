package main

import (
	"fmt"
	"html"
	"math"
	"strconv"
	"strings"
)

// svgStyle holds the chart colors for light and dark. A standalone chart carries it, along
// with its own background, so its text stays readable whatever page shows it. The report
// page holds it once for all of its charts.
const svgStyle = `.viz{--surface:#fcfcfb;--ink:#0b0b0b;--muted:#52514e;--grid:#e4e3df;--s1:#2a78d6;--s2:#eb6834;--band:#ecebe6;font-family:system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;font-size:13px}
@media (prefers-color-scheme:dark){.viz{--surface:#1a1a19;--ink:#f0efec;--muted:#c3c2b7;--grid:#3a3a37;--s1:#3987e5;--s2:#d95926;--band:#2b2b29}}
.viz .bg{fill:var(--surface)}.viz .band{fill:var(--band)}.viz .s1{fill:var(--s1)}.viz .s2{fill:var(--s2)}
.viz .ring{stroke:var(--surface);stroke-width:2}
.viz .t{fill:var(--ink)}.viz .m{fill:var(--muted)}.viz .h{fill:var(--ink);font-size:15px;font-weight:600}
.viz .g{stroke:var(--grid);stroke-width:1}.viz .lim{stroke:var(--ink);stroke-width:1.5;stroke-dasharray:5 4}`

// canvas builds one chart. Every mark carries a title with its real value, which is the
// hover text, and all marks are separate: no line ever joins two points, because a line
// would suggest values that were not measured.
type canvas struct {
	width, height int
	title         string
	body          strings.Builder
}

func newCanvas(width, height int, title string) *canvas {
	c := &canvas{width: width, height: height, title: title}
	fmt.Fprintf(&c.body, `<rect class="bg" width="%d" height="%d" rx="8"/>`, width, height)
	c.text(16, 26, "h", "start", title)
	return c
}

func (c *canvas) text(x, y float64, class, anchor, value string) {
	fmt.Fprintf(&c.body, `<text x="%s" y="%s" class="%s" text-anchor="%s">%s</text>`, num(x), num(y), class, anchor, html.EscapeString(value))
}

func (c *canvas) rect(x, y, w, h float64, class, tooltip string) {
	fmt.Fprintf(&c.body, `<rect x="%s" y="%s" width="%s" height="%s" rx="2" class="%s"><title>%s</title></rect>`, num(x), num(y), num(w), num(h), class, html.EscapeString(tooltip))
}

func (c *canvas) dot(x, y float64, class, tooltip string) {
	fmt.Fprintf(&c.body, `<circle cx="%s" cy="%s" r="5" class="%s ring"><title>%s</title></circle>`, num(x), num(y), class, html.EscapeString(tooltip))
}

func (c *canvas) line(x1, y1, x2, y2 float64, class string) {
	fmt.Fprintf(&c.body, `<line x1="%s" y1="%s" x2="%s" y2="%s" class="%s"/>`, num(x1), num(y1), num(x2), num(y2), class)
}

// legend draws swatches from the right edge, so that identity is never color alone.
func (c *canvas) legend(y float64, items ...[2]string) {
	x := float64(16)
	for _, item := range items {
		c.rect(x, y-9, 10, 10, item[0], item[1])
		c.text(x+15, y, "m", "start", item[1])
		x += 15 + 7.2*float64(len(item[1])) + 16
	}
}

func (c *canvas) svg(style string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" class="viz" viewBox="0 0 %d %d" role="img" aria-label="%s">`, c.width, c.height, html.EscapeString(c.title))
	if style != "" {
		fmt.Fprintf(&b, "<style>%s</style>", style)
	}
	b.WriteString(c.body.String())
	b.WriteString("</svg>\n")
	return b.String()
}

func num(v float64) string {
	return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64)
}

// niceCeil returns a round axis maximum at or above value.
func niceCeil(value float64) float64 {
	if value <= 0 {
		return 1
	}
	magnitude := math.Pow(10, math.Floor(math.Log10(value)))
	for _, step := range []float64{1, 2, 5, 10} {
		if value <= step*magnitude {
			return step * magnitude
		}
	}
	return 10 * magnitude
}
