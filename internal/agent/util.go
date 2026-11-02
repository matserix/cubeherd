package agent

import "strconv"

func itoa(n int) string     { return strconv.Itoa(n) }
func quote(s string) string { return strconv.Quote(s) }
