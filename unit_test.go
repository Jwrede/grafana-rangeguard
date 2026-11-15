package main

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

const day = 24 * time.Hour

func TestParseDur(t *testing.T) {
	for in, want := range map[string]time.Duration{"90d": 90 * day, "1h30m": 90 * time.Minute, "2w": 14 * day, "1y": 365 * day, "500ms": 500 * time.Millisecond, "": 0, "0": 0, "1.5h": 90 * time.Minute} {
		if got, err := parseDur(in); err != nil || got != want {
			t.Errorf("parseDur(%q) = %v, %v, want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"5", "h", "5x", "$__interval", "1h-"} {
		if _, err := parseDur(in); err == nil {
			t.Errorf("parseDur(%q) should fail", in)
		}
	}
}

func TestGrafanaTime(t *testing.T) {
	cases := []struct {
		in   string
		up   bool
		want time.Time
	}{
		{"1794744000000", false, time.UnixMilli(1794744000000)},
		{"now", false, t0},
		{"now-6h", false, t0.Add(-6 * time.Hour)},
		{"now-90d", false, t0.AddDate(0, 0, -90)},
		{"now-1M", false, t0.AddDate(0, -1, 0)},
		{"now/d", false, time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)},
		{"now/d", true, time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC).Add(-time.Millisecond)},
		{"now-1d/d", false, time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)},
		{"now/w", false, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)},
		{"now/y", false, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"2026-08-01T00:00:00.000Z", false, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got, err := grafanaTime(c.in, t0, c.up)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("grafanaTime(%q, up=%v) = %v, %v, want %v", c.in, c.up, got, err, c.want)
		}
	}
	for _, in := range []string{"", "yesterday", "now-", "now-5", "now/", "now-5q", "now*2"} {
		if _, err := grafanaTime(in, t0, false); err == nil {
			t.Errorf("grafanaTime(%q) should fail", in)
		}
	}
}

func TestAPITime(t *testing.T) {
	for in, want := range map[string]string{"1794744000": "sec", "1794744000.5": "sec", "1794744000000000000": "ns", "2026-11-15T12:00:00Z": "rfc"} {
		got, kind, err := apiTime(in)
		if err != nil || kind != want {
			t.Errorf("apiTime(%q) kind %q, %v, want %q", in, kind, err, want)
		}
		back, _, _ := apiTime(fmtAPITime(got, kind))
		if !back.Equal(got) {
			t.Errorf("apiTime round trip of %q: %v != %v", in, back, got)
		}
	}
	if _, _, err := apiTime("soon"); err == nil {
		t.Error("apiTime should reject text")
	}
}

func TestExprReach(t *testing.T) {
	cases := []struct {
		expr string
		want time.Duration
		at   int
	}{
		{`up`, 0, 0},
		{`rate(http_requests_total[5m])`, 5 * time.Minute, 0},
		{`sum_over_time(x[30d]) + rate(y[5m])`, 30 * day, 0},
		{`rate(x[5m] offset 1w)`, 7*day + 5*time.Minute, 0},
		{`max_over_time(rate(x[5m])[30d:1h])`, 30*day + 5*time.Minute, 0},
		{`x{path=~"/a[0-9]+[30d]"}`, 0, 0},
		{`rate(x[$__rate_interval])`, 0, 0},
		{`x @ 1600000000`, 0, 1},
		{`x @ end()`, 0, 0},
		{`count_over_time({app="a"} |= "offset 9d [9d]" [1h])`, time.Hour, 0},
		{"x{a=`[9d]`} # [9d]\n", 0, 0},
		{`sum by (offset) (rate(x[1m]))`, time.Minute, 0},
	}
	for _, c := range cases {
		got, at := exprReach(c.expr)
		if got != c.want || len(at) != c.at {
			t.Errorf("exprReach(%q) = %v, %d @, want %v, %d", c.expr, got, len(at), c.want, c.at)
		}
	}
}

func TestDecide(t *testing.T) {
	lim := Limits{MaxRange: 7 * day, MaxLookback: 90 * day, Rules: []string{"r"}}
	// Within limits: unchanged.
	if d := decide(lim, t0.Add(-day), t0, t0, 0); d.Changed || d.Reject {
		t.Errorf("within limits: %+v", d)
	}
	// Too long: keeps the newest part.
	d := decide(lim, t0.Add(-30*day), t0, t0, 0)
	if !d.Changed || d.Reject || !d.From.Equal(t0.Add(-7*day)) || !d.To.Equal(t0) {
		t.Errorf("clamp: %+v", d)
	}
	if !strings.Contains(d.Reason, "covers 30d") || !strings.Contains(d.Reason, "limit is 7d") {
		t.Errorf("reason: %s", d.Reason)
	}
	// Reject action.
	rej := lim
	rej.Reject = true
	if d := decide(rej, t0.Add(-30*day), t0, t0, 0); !d.Reject || d.Changed || !d.From.Equal(t0.Add(-30*day)) {
		t.Errorf("reject: %+v", d)
	}
	// Lookback: an old window is moved up to the allowed start.
	d = decide(Limits{MaxLookback: 90 * day}, t0.Add(-100*day), t0.Add(-80*day), t0, 0)
	if !d.Changed || !d.From.Equal(t0.Add(-90*day)) || !d.To.Equal(t0.Add(-80*day)) {
		t.Errorf("lookback clamp: %+v", d)
	}
	// Entirely before the lookback: cannot be shortened.
	if d := decide(Limits{MaxLookback: 90 * day}, t0.Add(-200*day), t0.Add(-100*day), t0, 0); !d.Reject {
		t.Errorf("before lookback: %+v", d)
	}
	// Range selectors count: 6d window with a 3d selector under a 7d limit.
	d = decide(lim, t0.Add(-6*day), t0, t0, 3*day)
	if !d.Changed || !d.From.Equal(t0.Add(-4*day)) {
		t.Errorf("selector: %+v", d)
	}
	// A selector longer than the limit cannot be fixed by shortening.
	if d := decide(lim, t0.Add(-time.Hour), t0, t0, 30*day); !d.Reject {
		t.Errorf("long selector: %+v", d)
	}
	// Instant query (from == to) within limits.
	if d := decide(lim, t0, t0, t0, 5*time.Minute); d.Changed || d.Reject {
		t.Errorf("instant: %+v", d)
	}
	// Unlimited and inverted ranges pass.
	if d := decide(Limits{}, t0.Add(-999*day), t0, t0, 0); d.Changed || d.Reject {
		t.Errorf("unlimited: %+v", d)
	}
	if d := decide(lim, t0, t0.Add(-30*day), t0, 0); d.Changed || d.Reject {
		t.Errorf("inverted: %+v", d)
	}
	// Both limits at once.
	d = decide(lim, t0.Add(-200*day), t0, t0, 0)
	if !d.From.Equal(t0.Add(-7*day)) || !strings.Contains(d.Reason, "200d") {
		t.Errorf("both: %+v", d)
	}
}

func TestMerge(t *testing.T) {
	m := merge(Limits{MaxRange: 7 * day, Rules: []string{"a"}}, Limits{MaxRange: day, MaxLookback: 30 * day, Reject: true, Rules: []string{"b"}})
	if m.MaxRange != day || m.MaxLookback != 30*day || !m.Reject || len(m.Rules) != 2 {
		t.Errorf("merge: %+v", m)
	}
	if m := merge(Limits{}, Limits{MaxRange: day}); m.MaxRange != day {
		t.Errorf("merge with unlimited: %+v", m)
	}
}

func TestConfig(t *testing.T) {
	good := `
upstream: http://grafana:3000
defaults: {max_range: 30d}
rules:
  - name: admins
    match: {role: [Admin]}
  - match: {datasource_type: [loki], role: [Viewer, anonymous]}
    max_range: 24h
    action: reject
`
	c, err := parseConfig([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if !c.needsIdentity || c.Rules[1].Name != "rule-2" || c.Rules[1].maxRange != day || c.Listen != ":3001" {
		t.Errorf("config: %+v", c)
	}
	viewer := Subject{Role: "Viewer"}
	if l := c.limitsFor(viewer, DS{UID: "x", Type: "loki", Known: true}); !l.Reject || l.MaxRange != day {
		t.Errorf("loki viewer: %+v", l)
	}
	if l := c.limitsFor(viewer, DS{UID: "x", Type: "mysql", Known: true}); l.Reject || l.MaxRange != 30*day || l.Rules[0] != "defaults" {
		t.Errorf("mysql viewer: %+v", l)
	}
	if l := c.limitsFor(Subject{Role: "admin"}, DS{Type: "loki"}); !l.unlimited() {
		t.Errorf("admin: %+v", l)
	}
	bad := map[string]string{
		"missing upstream":  `listen: ":1"`,
		"unknown key":       "upstream: http://g:3000\nmax_rnge: 1d",
		"bad duration":      "upstream: http://g:3000\ndefaults: {max_range: 1x}",
		"bad action":        "upstream: http://g:3000\ndefaults: {action: drop}",
		"bad role":          "upstream: http://g:3000\nrules: [{match: {role: [Viwer]}}]",
		"bad mode":          "upstream: http://g:3000\nmode: influx",
		"role in prom mode": "upstream: http://p:9090\nmode: prometheus\nrules: [{match: {role: [Viewer]}}]",
	}
	for name, raw := range bad {
		if _, err := parseConfig([]byte(raw)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	t.Setenv("RG_TOKEN", "secret")
	c, err = parseConfig([]byte("upstream: http://g:3000\ngrafana_token: ${RG_TOKEN}"))
	if err != nil || c.GrafanaToken != "secret" {
		t.Errorf("env expansion: %v %+v", err, c)
	}
}
