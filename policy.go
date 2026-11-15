package main

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen        string `yaml:"listen"`
	MetricsListen string `yaml:"metrics_listen"`
	// Mode is "grafana" (proxy in front of Grafana), "prometheus" or "loki"
	// (proxy in front of a Prometheus or Loki compatible HTTP API).
	Mode         string `yaml:"mode"`
	Upstream     string `yaml:"upstream"`
	GrafanaToken string `yaml:"grafana_token"`
	DryRun       bool   `yaml:"dry_run"`
	UserHeader   string `yaml:"user_header"`
	Defaults     Limit  `yaml:"defaults"`
	Rules        []Rule `yaml:"rules"`

	upstream      *url.URL
	needsIdentity bool
}

type Limit struct {
	MaxRange    string `yaml:"max_range"`
	MaxLookback string `yaml:"max_lookback"`
	Action      string `yaml:"action"`

	maxRange, maxLookback time.Duration
}

type Rule struct {
	Name  string `yaml:"name"`
	Match Match  `yaml:"match"`
	Limit `yaml:",inline"`
}

type Match struct {
	DatasourceUID  []string `yaml:"datasource_uid"`
	DatasourceType []string `yaml:"datasource_type"`
	DashboardUID   []string `yaml:"dashboard_uid"`
	Role           []string `yaml:"role"`
	User           []string `yaml:"user"`
	Org            []int64  `yaml:"org"`
}

// Subject describes who sent a request.
type Subject struct {
	Role      string // Admin, Editor, Viewer, None, anonymous, public, unknown
	User      string
	Org       int64
	Dashboard string
}

// DS is the data source a query is addressed to. Known is false when the
// request does not name one (public dashboards, upstream modes).
type DS struct {
	UID, Type string
	Known     bool
}

// Limits are the effective limits for one request.
type Limits struct {
	MaxRange, MaxLookback time.Duration
	Reject                bool
	Rules                 []string
}

func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseConfig(raw)
}

func parseConfig(raw []byte) (*Config, error) {
	c := &Config{}
	dec := yaml.NewDecoder(bytes.NewReader([]byte(os.Expand(string(raw), envOnly))))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if c.Listen == "" {
		c.Listen = ":3001"
	}
	if c.Mode == "" {
		c.Mode = "grafana"
	}
	if c.Mode != "grafana" && c.Mode != "prometheus" && c.Mode != "loki" {
		return nil, fmt.Errorf("config: mode must be grafana, prometheus or loki, got %q", c.Mode)
	}
	if c.UserHeader == "" {
		c.UserHeader = "X-Grafana-User"
	}
	u, err := url.Parse(c.Upstream)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("config: upstream must be a URL such as http://grafana:3000, got %q", c.Upstream)
	}
	c.upstream = u
	if err := c.Defaults.compile("defaults"); err != nil {
		return nil, err
	}
	for i := range c.Rules {
		r := &c.Rules[i]
		if r.Name == "" {
			r.Name = fmt.Sprintf("rule-%d", i+1)
		}
		if err := r.Limit.compile("rule " + r.Name); err != nil {
			return nil, err
		}
		m := r.Match
		if len(m.Role)+len(m.User)+len(m.Org) > 0 {
			c.needsIdentity = true
		}
		if c.Mode != "grafana" && len(m.DatasourceUID)+len(m.DatasourceType)+len(m.DashboardUID)+len(m.Role)+len(m.Org) > 0 {
			return nil, fmt.Errorf("config: rule %s: mode %s can only match on user", r.Name, c.Mode)
		}
		for _, role := range m.Role {
			switch strings.ToLower(role) {
			case "admin", "editor", "viewer", "none", "anonymous", "public", "unknown":
			default:
				return nil, fmt.Errorf("config: rule %s: unknown role %q", r.Name, role)
			}
		}
	}
	return c, nil
}

// envOnly expands ${VAR} and leaves a single "$" (as in "$__interval") alone.
func envOnly(name string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return "$" + name
}

func (l *Limit) compile(where string) error {
	var err error
	if l.maxRange, err = parseDur(l.MaxRange); err != nil {
		return fmt.Errorf("config: %s: max_range: %w", where, err)
	}
	if l.maxLookback, err = parseDur(l.MaxLookback); err != nil {
		return fmt.Errorf("config: %s: max_lookback: %w", where, err)
	}
	switch l.Action {
	case "":
		l.Action = "clamp"
	case "clamp", "reject":
	default:
		return fmt.Errorf("config: %s: action must be clamp or reject, got %q", where, l.Action)
	}
	return nil
}

func (m Match) String() string {
	var parts []string
	add := func(k string, v []string) {
		if len(v) > 0 {
			parts = append(parts, k+"="+strings.Join(v, "|"))
		}
	}
	add("datasource_uid", m.DatasourceUID)
	add("datasource_type", m.DatasourceType)
	add("dashboard_uid", m.DashboardUID)
	add("role", m.Role)
	add("user", m.User)
	if len(m.Org) > 0 {
		parts = append(parts, strings.Trim(strings.ReplaceAll(fmt.Sprint("org=", m.Org), " ", "|"), "[]"))
	}
	if len(parts) == 0 {
		return "everything"
	}
	return strings.Join(parts, " ")
}

func inFold(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

func (m Match) matches(s Subject, ds DS) bool {
	if len(m.DatasourceUID) > 0 && !(ds.Known && inFold(m.DatasourceUID, ds.UID)) {
		return false
	}
	if len(m.DatasourceType) > 0 && !(ds.Type != "" && inFold(m.DatasourceType, ds.Type)) {
		return false
	}
	if len(m.DashboardUID) > 0 && !(s.Dashboard != "" && inFold(m.DashboardUID, s.Dashboard)) {
		return false
	}
	if len(m.Role) > 0 && !inFold(m.Role, s.Role) {
		return false
	}
	if len(m.User) > 0 && !(s.User != "" && inFold(m.User, s.User)) {
		return false
	}
	if len(m.Org) > 0 {
		ok := false
		for _, o := range m.Org {
			ok = ok || o == s.Org
		}
		if !ok {
			return false
		}
	}
	return true
}

// limitsFor returns the limits of the first matching rule, or the defaults.
func (c *Config) limitsFor(s Subject, ds DS) Limits {
	for _, r := range c.Rules {
		if r.Match.matches(s, ds) {
			return Limits{r.maxRange, r.maxLookback, r.Action == "reject", []string{r.Name}}
		}
	}
	return Limits{c.Defaults.maxRange, c.Defaults.maxLookback, c.Defaults.Action == "reject", []string{"defaults"}}
}

// merge combines the limits of several queries in one request: the
// smallest non-zero limit wins and one rejecting rule rejects the request.
func merge(a, b Limits) Limits {
	pick := func(x, y time.Duration) time.Duration {
		if x == 0 || (y != 0 && y < x) {
			return y
		}
		return x
	}
	out := Limits{pick(a.MaxRange, b.MaxRange), pick(a.MaxLookback, b.MaxLookback), a.Reject || b.Reject, a.Rules}
	for _, r := range b.Rules {
		if !inFold(out.Rules, r) {
			out.Rules = append(out.Rules, r)
		}
	}
	return out
}

func (l Limits) unlimited() bool { return l.MaxRange == 0 && l.MaxLookback == 0 }

// Decision is the result of applying limits to one time range.
type Decision struct {
	From, To time.Time
	Changed  bool
	Reject   bool
	Reason   string
}

// decide applies the limits to [from, to]. extra is how far the query
// reads before from (range selectors, offsets). When the range cannot be
// shortened to fit, the request is rejected regardless of the action.
func decide(l Limits, from, to, now time.Time, extra time.Duration) Decision {
	d := Decision{From: from, To: to}
	rule := strings.Join(l.Rules, ",")
	if l.unlimited() {
		return d
	}
	if to.Before(from) {
		return d
	}
	var reasons []string
	if l.MaxLookback > 0 {
		earliest := now.Add(-l.MaxLookback)
		if d.From.Add(-extra).Before(earliest) {
			reasons = append(reasons, fmt.Sprintf("the query reads data from %s ago, the limit is %s", fmtDur(now.Sub(d.From.Add(-extra))), fmtDur(l.MaxLookback)))
			nf := earliest.Add(extra)
			if nf.After(d.To) {
				d.Reject = true
			} else {
				d.From, d.Changed = nf, true
			}
		}
	}
	if l.MaxRange > 0 && !d.Reject && d.To.Sub(d.From)+extra > l.MaxRange {
		reasons = append(reasons, fmt.Sprintf("the query covers %s, the limit is %s", fmtDur(to.Sub(from)+extra), fmtDur(l.MaxRange)))
		if extra > l.MaxRange {
			d.Reject = true
		} else {
			d.From, d.Changed = d.To.Add(-(l.MaxRange - extra)), true
		}
	}
	if len(reasons) == 0 {
		return d
	}
	if l.Reject {
		d.Reject = true
	}
	d.Reason = strings.Join(reasons, "; ")
	switch {
	case d.Reject && d.Changed || d.Reject && l.Reject:
		d.Reason = "time range rejected: " + d.Reason
	case d.Reject:
		d.Reason = "time range rejected, it cannot be shortened to fit: " + d.Reason
	default:
		d.Reason = fmt.Sprintf("time range shortened to %s: %s", fmtDur(d.To.Sub(d.From)), d.Reason)
	}
	d.Reason += " (rangeguard rule " + rule + ")"
	if d.Reject {
		d.From, d.To, d.Changed = from, to, false
	}
	return d
}
