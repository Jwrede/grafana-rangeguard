package main

import (
	"strconv"
	"strings"
	"time"
)

// exprReach inspects a PromQL or LogQL expression. extra is an upper bound
// for how far the expression reads before the evaluation time: the sum of
// all subquery ranges, plus the largest range selector, plus the largest
// offset. at lists the timestamps of numeric @ modifiers.
// Durations that are not literals (Grafana variables) are ignored.
func exprReach(expr string) (extra time.Duration, at []time.Time) {
	var sub, sel, off time.Duration
	i := 0
	n := len(expr)
	for i < n {
		c := expr[i]
		switch {
		case c == '"' || c == '\'' || c == '`':
			i++
			for i < n && expr[i] != c {
				if expr[i] == '\\' && c != '`' {
					i++
				}
				i++
			}
			i++
		case c == '#':
			for i < n && expr[i] != '\n' {
				i++
			}
		case c == '[':
			j := strings.IndexByte(expr[i:], ']')
			if j < 0 {
				return sub + sel + off, at
			}
			inner := expr[i+1 : i+j]
			if k := strings.IndexByte(inner, ':'); k >= 0 {
				if d, err := parseDur(inner[:k]); err == nil {
					sub += d
				}
			} else if d, err := parseDur(inner); err == nil && d > sel {
				sel = d
			}
			i += j + 1
		case c == '@':
			j := i + 1
			for j < n && (expr[j] == ' ' || expr[j] == '\t' || expr[j] == '\n') {
				j++
			}
			k := j
			for k < n && (expr[k] >= '0' && expr[k] <= '9' || expr[k] == '.' || expr[k] == '-' || expr[k] == '+' || expr[k] == 'e') {
				k++
			}
			if k > j {
				if f, err := strconv.ParseFloat(expr[j:k], 64); err == nil {
					at = append(at, time.UnixMilli(int64(f*1000)))
				}
			}
			i = k
			if k == j {
				i = j + 1
			}
		case isWord(c):
			j := i
			for j < n && isWord(expr[j]) {
				j++
			}
			if expr[i:j] == "offset" {
				k := j
				for k < n && (expr[k] == ' ' || expr[k] == '\t' || expr[k] == '\n') {
					k++
				}
				m := k
				for m < n && (isWord(expr[m]) || expr[m] == '.') {
					m++
				}
				if d, err := parseDur(expr[k:m]); err == nil && d > off {
					off = d
				}
				j = m
			}
			i = j
		default:
			i++
		}
	}
	return sub + sel + off, at
}

func isWord(c byte) bool {
	return c == '_' || c == ':' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
