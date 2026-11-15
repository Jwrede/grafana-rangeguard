package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// parseDur parses durations such as "90d", "12h", "1h30m", "2w", "1y".
func parseDur(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	var total time.Duration
	i := 0
	for i < len(s) {
		j := i
		for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.') {
			j++
		}
		if j == i {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		n, err := strconv.ParseFloat(s[i:j], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		k := j
		for k < len(s) && (s[k] < '0' || s[k] > '9') && s[k] != '.' {
			k++
		}
		var unit time.Duration
		switch s[j:k] {
		case "ms":
			unit = time.Millisecond
		case "s":
			unit = time.Second
		case "m":
			unit = time.Minute
		case "h":
			unit = time.Hour
		case "d":
			unit = 24 * time.Hour
		case "w":
			unit = 7 * 24 * time.Hour
		case "y":
			unit = 365 * 24 * time.Hour
		default:
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		total += time.Duration(n * float64(unit))
		i = k
	}
	return total, nil
}

func fmtDur(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	d = d.Round(time.Second)
	out := ""
	for _, u := range []struct {
		n string
		d time.Duration
	}{{"d", 24 * time.Hour}, {"h", time.Hour}, {"m", time.Minute}, {"s", time.Second}} {
		if d >= u.d {
			out += fmt.Sprintf("%d%s", d/u.d, u.n)
			d %= u.d
		}
	}
	return out
}

// grafanaTime parses the time values Grafana sends in query requests:
// epoch milliseconds, RFC 3339, or relative expressions such as "now-6h",
// "now-1d/d" and "now/w". Rounding uses UTC. roundUp selects the end of
// the unit, which is what Grafana does for the "to" value.
func grafanaTime(s string, now time.Time, roundUp bool) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty time value")
	}
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.UnixMilli(ms), nil
	}
	if strings.HasPrefix(s, "now") {
		t := now.UTC()
		rest := s[3:]
		for rest != "" {
			switch rest[0] {
			case '+', '-':
				j := 1
				for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
					j++
				}
				if j == 1 || j >= len(rest) {
					return time.Time{}, fmt.Errorf("invalid time value %q", s)
				}
				n, _ := strconv.Atoi(rest[1:j])
				if rest[0] == '-' {
					n = -n
				}
				var err error
				if t, err = addUnit(t, n, rest[j]); err != nil {
					return time.Time{}, fmt.Errorf("invalid time value %q", s)
				}
				rest = rest[j+1:]
			case '/':
				if len(rest) < 2 {
					return time.Time{}, fmt.Errorf("invalid time value %q", s)
				}
				var err error
				if t, err = roundUnit(t, rest[1], roundUp); err != nil {
					return time.Time{}, fmt.Errorf("invalid time value %q", s)
				}
				rest = rest[2:]
			default:
				return time.Time{}, fmt.Errorf("invalid time value %q", s)
			}
		}
		return t, nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid time value %q", s)
}

func addUnit(t time.Time, n int, u byte) (time.Time, error) {
	switch u {
	case 's':
		return t.Add(time.Duration(n) * time.Second), nil
	case 'm':
		return t.Add(time.Duration(n) * time.Minute), nil
	case 'h':
		return t.Add(time.Duration(n) * time.Hour), nil
	case 'd':
		return t.AddDate(0, 0, n), nil
	case 'w':
		return t.AddDate(0, 0, 7*n), nil
	case 'M':
		return t.AddDate(0, n, 0), nil
	case 'Q':
		return t.AddDate(0, 3*n, 0), nil
	case 'y':
		return t.AddDate(n, 0, 0), nil
	}
	return t, fmt.Errorf("unit")
}

func roundUnit(t time.Time, u byte, up bool) (time.Time, error) {
	var start, next time.Time
	y, mo, d := t.Date()
	switch u {
	case 's':
		start = t.Truncate(time.Second)
		next = start.Add(time.Second)
	case 'm':
		start = t.Truncate(time.Minute)
		next = start.Add(time.Minute)
	case 'h':
		start = t.Truncate(time.Hour)
		next = start.Add(time.Hour)
	case 'd':
		start = time.Date(y, mo, d, 0, 0, 0, 0, time.UTC)
		next = start.AddDate(0, 0, 1)
	case 'w':
		wd := (int(t.Weekday()) + 6) % 7
		start = time.Date(y, mo, d-wd, 0, 0, 0, 0, time.UTC)
		next = start.AddDate(0, 0, 7)
	case 'M':
		start = time.Date(y, mo, 1, 0, 0, 0, 0, time.UTC)
		next = start.AddDate(0, 1, 0)
	case 'Q':
		start = time.Date(y, time.Month((int(mo)-1)/3*3+1), 1, 0, 0, 0, 0, time.UTC)
		next = start.AddDate(0, 3, 0)
	case 'y':
		start = time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)
		next = start.AddDate(1, 0, 0)
	default:
		return t, fmt.Errorf("unit")
	}
	if up {
		return next.Add(-time.Millisecond), nil
	}
	return start, nil
}

// apiTime parses a Prometheus or Loki API timestamp. kind is "rfc", "sec"
// or "ns" and is used to write a clamped value back in the same form.
func apiTime(s string) (time.Time, string, error) {
	s = strings.TrimSpace(s)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && len(strings.TrimLeft(s, "-")) > 12 {
		return time.Unix(0, n), "ns", nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
		sec, frac := math.Modf(f)
		return time.Unix(int64(sec), int64(frac*1e9)), "sec", nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, "rfc", nil
	}
	return time.Time{}, "", fmt.Errorf("invalid timestamp %q", s)
}

func fmtAPITime(t time.Time, kind string) string {
	switch kind {
	case "ns":
		return strconv.FormatInt(t.UnixNano(), 10)
	case "rfc":
		return t.UTC().Format(time.RFC3339Nano)
	}
	return strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', 3, 64)
}
