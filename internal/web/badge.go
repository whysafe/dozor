package web

import (
	"fmt"
	"html"
	"strings"

	"github.com/whysafe/dozor/internal/textru"
)

// Badge draws a shields.io-style SVG for READMEs and dashboards.
func Badge(label string, ratio float64, has bool) string {
	value, color := "нет данных", "#9f9f9f"
	if has {
		value = strings.Replace(textru.Percent(ratio), ",", ".", 1)
		switch {
		case ratio >= 0.999:
			color = "#2e7d4f"
		case ratio >= 0.99:
			color = "#7a9a1f"
		case ratio >= 0.95:
			color = "#c4880f"
		default:
			color = "#c0392b"
		}
	}
	lw, vw := textWidth(label), textWidth(value)
	total := lw + vw
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="20" role="img" aria-label="%s: %s">`+
		`<linearGradient id="s" x2="0" y2="100%%"><stop offset="0" stop-color="#bbb" stop-opacity=".1"/><stop offset="1" stop-opacity=".1"/></linearGradient>`+
		`<clipPath id="r"><rect width="%d" height="20" rx="3" fill="#fff"/></clipPath>`+
		`<g clip-path="url(#r)"><rect width="%d" height="20" fill="#555"/><rect x="%d" width="%d" height="20" fill="%s"/><rect width="%d" height="20" fill="url(#s)"/></g>`+
		`<g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="11">`+
		`<text x="%d" y="14">%s</text><text x="%d" y="14">%s</text></g></svg>`,
		total, html.EscapeString(label), value,
		total, lw, lw, vw, color, total,
		lw/2, html.EscapeString(label), lw+vw/2, html.EscapeString(value))
}

// textWidth approximates Verdana 11px glyph widths; exact metrics are not worth a font parser.
func textWidth(s string) int {
	w := 0.0
	for _, r := range s {
		switch {
		case strings.ContainsRune(".,:;!|il1 ", r):
			w += 3.8
		case r == '%' || r == 'm' || r == 'w':
			w += 10
		case r >= 'А' && r <= 'я':
			w += 7.4
		default:
			w += 7
		}
	}
	return int(w) + 12
}
