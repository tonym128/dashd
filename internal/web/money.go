package web

import (
	"fmt"
	"regexp"
)

var bigDollars = regexp.MustCompile(`\$\d{4,}`)

// groupDollars adds thousands separators to every dollar amount in s, so
// sentences built with "$%.2f" read the same as the tables.
func groupDollars(s string) string {
	return bigDollars.ReplaceAllStringFunc(s, func(m string) string {
		var n int64
		if _, err := fmt.Sscanf(m[1:], "%d", &n); err != nil {
			return m
		}
		return "$" + groupInt64(n)
	})
}

func usd(v float64) string { return groupDollars(fmt.Sprintf("$%.2f", v)) }

func sprintf4(v float64) string { return fmt.Sprintf("$%.4f", v) }
