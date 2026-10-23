// Package textru formats durations and counts in Russian.
package textru

import (
	"fmt"
	"strings"
	"time"
)

// Plural picks the Russian word form for n: one (1 минута), few (2 минуты), many (5 минут).
func Plural(n int64, one, few, many string) string {
	if n < 0 {
		n = -n
	}
	switch mod100 := n % 100; {
	case mod100 >= 11 && mod100 <= 14:
		return many
	case n%10 == 1:
		return one
	case n%10 >= 2 && n%10 <= 4:
		return few
	default:
		return many
	}
}

// Duration renders a duration with at most two units: "2 ч 15 мин", "40 с", "3 дн 4 ч".
func Duration(d time.Duration) string {
	if d < time.Second {
		return "меньше секунды"
	}
	d = d.Round(time.Second)
	days := int64(d / (24 * time.Hour))
	hours := int64(d/time.Hour) % 24
	minutes := int64(d/time.Minute) % 60
	seconds := int64(d/time.Second) % 60

	parts := []struct {
		n    int64
		unit string
	}{{days, "дн"}, {hours, "ч"}, {minutes, "мин"}, {seconds, "с"}}

	var out []string
	for i, p := range parts {
		if p.n == 0 {
			if len(out) > 0 {
				break
			}
			continue
		}
		out = append(out, fmt.Sprintf("%d %s", p.n, p.unit))
		if len(out) == 2 || i == len(parts)-1 {
			break
		}
	}
	return strings.Join(out, " ")
}

// Percent formats an uptime ratio without lying by rounding: 99.996% must not
// look like a perfect 100%, so values below 1 are truncated, not rounded.
func Percent(ratio float64) string {
	if ratio >= 1 {
		return "100%"
	}
	if ratio < 0 {
		ratio = 0
	}
	truncated := float64(int64(ratio*10000)) / 100
	s := fmt.Sprintf("%.2f", truncated)
	return strings.Replace(s, ".", ",", 1) + "%"
}

var monthsShort = [...]string{"янв", "фев", "мар", "апр", "мая", "июн", "июл", "авг", "сен", "окт", "ноя", "дек"}

// Date renders "7 окт".
func Date(t time.Time) string {
	return fmt.Sprintf("%d %s", t.Day(), monthsShort[t.Month()-1])
}

// DateTime renders "7 окт, 14:05".
func DateTime(t time.Time) string {
	return Date(t) + t.Format(", 15:04")
}
