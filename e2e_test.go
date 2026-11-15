package main

// End-to-end tests against real Grafana, Prometheus and Loki servers.
// They run when RG_E2E=1 is set, see e2e/run.sh.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var (
	grafanaURL = env("GRAFANA_URL", "http://127.0.0.1:3000")
	promURL    = env("PROM_URL", "http://127.0.0.1:9090")
	lokiURL    = env("LOKI_URL", "http://127.0.0.1:3100")
)

const tdType = "grafana-testdata-datasource"

func e2e(t *testing.T) {
	if os.Getenv("RG_E2E") != "1" {
		t.Skip("set RG_E2E=1 to run against real servers")
	}
}

// call sends a request, optionally with basic auth "user:password".
func call(t *testing.T, method, u, auth string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	ct := ""
	switch b := body.(type) {
	case nil:
	case url.Values:
		rd, ct = strings.NewReader(b.Encode()), "application/x-www-form-urlencoded"
	case string:
		rd, ct = strings.NewReader(b), "application/json"
	default:
		raw, _ := json.Marshal(b)
		rd, ct = bytes.NewReader(raw), "application/json"
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		t.Fatal(err)
	}
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if strings.HasPrefix(auth, "Bearer ") {
		req.Header.Set("Authorization", auth)
	} else if auth != "" {
		u, p, _ := strings.Cut(auth, ":")
		req.SetBasicAuth(u, p)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

type M = map[string]any

const (
	admin  = "admin:admin"
	viewer = "vera:Vera-pass-12345"
	editor = "ed:Ed-pass-12345"
)

// setupGrafana creates data sources and users. It is idempotent.
func setupGrafana(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		resp, err := http.Get(grafanaURL + "/api/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Grafana at %s did not become healthy", grafanaURL)
		}
		time.Sleep(2 * time.Second)
	}
	for _, ds := range []M{
		{"name": "td", "uid": "td", "type": tdType, "access": "proxy"},
		{"name": "prom", "uid": "prom", "type": "prometheus", "access": "proxy", "url": promURL},
	} {
		if code, out := call(t, "POST", grafanaURL+"/api/datasources", admin, ds); code != 200 && code != 409 {
			t.Fatalf("create data source %v: %d %s", ds["name"], code, out)
		}
	}
	for _, u := range []string{viewer, editor} {
		login, pw, _ := strings.Cut(u, ":")
		code, out := call(t, "POST", grafanaURL+"/api/admin/users", admin, M{"name": login, "login": login, "email": login + "@example.org", "password": pw})
		if code != 200 && code != 412 && code != 409 {
			t.Fatalf("create user %s: %d %s", login, code, out)
		}
	}
	_, out := call(t, "GET", grafanaURL+"/api/org/users", admin, nil)
	var users []struct {
		UserID int64  `json:"userId"`
		Login  string `json:"login"`
	}
	json.Unmarshal(out, &users)
	for _, u := range users {
		if u.Login == "ed" {
			if code, out := call(t, "PATCH", fmt.Sprintf("%s/api/org/users/%d", grafanaURL, u.UserID), admin, M{"role": "Editor"}); code != 200 {
				t.Fatalf("make ed an editor: %d %s", code, out)
			}
		}
	}
}

func e2eGuard(t *testing.T, mode, upstream, cfg string) string {
	t.Helper()
	c, err := parseConfig([]byte(fmt.Sprintf("mode: %s\nupstream: %s\n%s", mode, upstream, cfg)))
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(c)
	s.logf = t.Logf
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return ts.URL
}

func tdQuery(from, to time.Time) M {
	return M{"from": ms(from), "to": ms(to), "queries": []M{{
		"refId": "A", "datasource": M{"uid": "td", "type": tdType}, "scenarioId": "random_walk",
		"intervalMs": 3600000, "maxDataPoints": 5000,
	}}}
}

type frames struct {
	Message string
	Results map[string]struct {
		Error  string
		Frames []struct {
			Schema struct {
				Meta struct {
					Notices []struct{ Severity, Text string }
				}
			}
			Data struct {
				Values []json.RawMessage
			}
		}
	}
}

// span returns the first timestamp of the first frame and its notices.
func span(t *testing.T, code int, out []byte) (time.Time, string) {
	t.Helper()
	var f frames
	if err := json.Unmarshal(out, &f); err != nil || code != 200 {
		t.Fatalf("query failed: %d %.600s", code, out)
	}
	fr := f.Results["A"].Frames
	if len(fr) == 0 || len(fr[0].Data.Values) == 0 {
		t.Fatalf("no data in response: %.600s", out)
	}
	var ts []float64
	if err := json.Unmarshal(fr[0].Data.Values[0], &ts); err != nil || len(ts) == 0 {
		t.Fatalf("no timestamps in response: %.600s", out)
	}
	minTS := ts[0]
	for _, v := range ts {
		if v < minTS {
			minTS = v
		}
	}
	notice := ""
	for _, n := range fr[0].Schema.Meta.Notices {
		notice += n.Severity + ": " + n.Text
	}
	return time.UnixMilli(int64(minTS)), notice
}

func near(t *testing.T, what string, got, want time.Time, slack time.Duration) {
	t.Helper()
	if d := got.Sub(want); d < -slack || d > slack {
		t.Errorf("%s: data starts at %s, want %s (off by %s)", what, got.UTC().Format(time.RFC3339), want.UTC().Format(time.RFC3339), d)
	}
}

const e2eRules = `
rules:
  - name: admins
    match: {role: [Admin]}
  - name: prometheus
    match: {datasource_type: [prometheus]}
    max_range: 6h
  - name: viewers
    match: {role: [Viewer, anonymous]}
    max_range: 7d
defaults: {max_range: 30d}
`

func TestE2EGrafanaRoles(t *testing.T) {
	e2e(t)
	setupGrafana(t)
	g := e2eGuard(t, "grafana", grafanaURL, e2eRules)
	now := time.Now()
	body := tdQuery(now.Add(-90*day), now)

	// Baseline without the proxy: the test data source returns the full range.
	code, out := call(t, "POST", grafanaURL+"/api/ds/query", viewer, body)
	first, notice := span(t, code, out)
	near(t, "direct", first, now.Add(-90*day), 2*time.Hour)
	if notice != "" {
		t.Errorf("direct: unexpected notice %q", notice)
	}

	for _, c := range []struct {
		name, auth string
		want       time.Duration
		notice     string
	}{
		{"anonymous", "", 7 * day, "warning: time range shortened to 7d"},
		{"viewer", viewer, 7 * day, "warning: time range shortened to 7d"},
		{"editor", editor, 30 * day, "warning: time range shortened to 30d"},
		{"admin", admin, 90 * day, ""},
	} {
		code, out := call(t, "POST", g+"/api/ds/query", c.auth, body)
		first, notice := span(t, code, out)
		near(t, c.name, first, now.Add(-c.want), 2*time.Hour)
		if !strings.HasPrefix(notice, c.notice) || (c.notice == "") != (notice == "") {
			t.Errorf("%s: notice %q, want prefix %q", c.name, notice, c.notice)
		}
	}

	// Relative time values, as API clients send them.
	rel := tdQuery(now, now)
	rel["from"], rel["to"] = "now-60d", "now"
	code, out = call(t, "POST", g+"/api/ds/query", viewer, rel)
	first, _ = span(t, code, out)
	near(t, "relative", first, now.Add(-7*day), 2*time.Hour)

	// The rest of Grafana works through the proxy.
	if code, out := call(t, "GET", g+"/api/org", admin, nil); code != 200 || !strings.Contains(string(out), `"name"`) {
		t.Errorf("GET /api/org through the proxy: %d %s", code, out)
	}
	if code, out := call(t, "GET", g+"/login", "", nil); code != 200 || !strings.Contains(string(out), "<html") {
		t.Errorf("GET /login through the proxy: %d %.100s", code, out)
	}
}

func TestE2EGrafanaReject(t *testing.T) {
	e2e(t)
	setupGrafana(t)
	g := e2eGuard(t, "grafana", grafanaURL, "rules:\n  - {name: td, match: {datasource_uid: [td]}, max_range: 1d, max_lookback: 30d, action: reject}\n")
	now := time.Now()
	code, out := call(t, "POST", g+"/api/ds/query", admin, tdQuery(now.Add(-3*day), now))
	var f frames
	json.Unmarshal(out, &f)
	if code != 400 || !strings.Contains(f.Message, "the limit is 1d") || f.Results["A"].Error == "" {
		t.Errorf("reject: %d %s", code, out)
	}
	code, out = call(t, "POST", g+"/api/ds/query", admin, tdQuery(now.Add(-40*day), now.Add(-39*day-12*time.Hour)))
	if code != 400 || !strings.Contains(string(out), "the limit is 30d") {
		t.Errorf("lookback reject: %d %s", code, out)
	}
	code, out = call(t, "POST", g+"/api/ds/query", admin, tdQuery(now.Add(-12*time.Hour), now))
	first, notice := span(t, code, out)
	near(t, "within limit", first, now.Add(-12*time.Hour), 2*time.Hour)
	if notice != "" {
		t.Errorf("within limit: notice %q", notice)
	}
}

func promQuery(from, to time.Time, typ string) M {
	return M{"from": ms(from), "to": ms(to), "queries": []M{{
		"refId": "A", "datasource": M{"uid": "prom", "type": typ}, "expr": "time()", "range": true, "instant": false,
		"intervalMs": 600000, "maxDataPoints": 2000,
	}}}
}

func TestE2EGrafanaPrometheus(t *testing.T) {
	e2e(t)
	setupGrafana(t)
	now := time.Now()
	body := promQuery(now.Add(-3*day), now, "prometheus")
	code, out := call(t, "POST", grafanaURL+"/api/ds/query", viewer, body)
	first, _ := span(t, code, out)
	near(t, "direct", first, now.Add(-3*day), time.Hour)

	g := e2eGuard(t, "grafana", grafanaURL, e2eRules)
	code, out = call(t, "POST", g+"/api/ds/query", viewer, body)
	first, notice := span(t, code, out)
	near(t, "through proxy", first, now.Add(-6*time.Hour), time.Hour)
	if !strings.Contains(notice, "shortened to 6h") {
		t.Errorf("notice: %q", notice)
	}

	// A range selector that is longer than the limit.
	long := promQuery(now.Add(-time.Hour), now, "prometheus")
	long["queries"].([]M)[0]["expr"] = "count_over_time(up[30d])"
	if code, out := call(t, "POST", g+"/api/ds/query", viewer, long); code != 400 || !strings.Contains(string(out), "cannot be shortened") {
		t.Errorf("long selector: %d %s", code, out)
	}

	// Data source proxy path with Prometheus parameters.
	g = e2eGuard(t, "grafana", grafanaURL, "rules:\n  - {match: {datasource_uid: [prom]}, max_range: 2h}\n")
	u := fmt.Sprintf("/api/datasources/proxy/uid/prom/api/v1/query_range?query=time()&start=%d&end=%d&step=600", now.Add(-3*day).Unix(), now.Unix())
	code, out = call(t, "GET", grafanaURL+u, admin, nil)
	first, _ = promSpan(t, code, out)
	near(t, "proxy path direct", first, now.Add(-3*day), time.Hour)
	code, out = call(t, "GET", g+u, admin, nil)
	first, warn := promSpan(t, code, out)
	near(t, "proxy path guarded", first, now.Add(-2*time.Hour), 15*time.Minute)
	if !strings.Contains(warn, "shortened to 2h") {
		t.Errorf("proxy path warning: %q", warn)
	}
}

func TestE2EGrafanaTokenResolvesType(t *testing.T) {
	e2e(t)
	setupGrafana(t)
	code, out := call(t, "POST", grafanaURL+"/api/serviceaccounts", admin, M{"name": fmt.Sprintf("rangeguard-%d", time.Now().UnixNano()), "role": "Admin"})
	var sa struct{ ID int64 }
	json.Unmarshal(out, &sa)
	if code != 201 && code != 200 {
		t.Fatalf("create service account: %d %s", code, out)
	}
	code, out = call(t, "POST", fmt.Sprintf("%s/api/serviceaccounts/%d/tokens", grafanaURL, sa.ID), admin, M{"name": "t"})
	var tok struct{ Key string }
	json.Unmarshal(out, &tok)
	if tok.Key == "" {
		t.Fatalf("create token: %d %s", code, out)
	}
	g := e2eGuard(t, "grafana", grafanaURL, "grafana_token: "+tok.Key+"\nrules:\n  - {match: {datasource_type: [prometheus]}, max_range: 6h}\n")
	now := time.Now()
	// The request names another type. Grafana routes by uid, so does the rule.
	code, out = call(t, "POST", g+"/api/ds/query", viewer, promQuery(now.Add(-3*day), now, "mysql"))
	first, _ := span(t, code, out)
	near(t, "resolved type", first, now.Add(-6*time.Hour), time.Hour)
	// The token itself can query through the proxy.
	code, out = call(t, "POST", g+"/api/ds/query", "Bearer "+tok.Key, promQuery(now.Add(-3*day), now, "prometheus"))
	first, _ = span(t, code, out)
	near(t, "token as client", first, now.Add(-6*time.Hour), time.Hour)
}

func TestE2EGrafanaPublicDashboard(t *testing.T) {
	e2e(t)
	setupGrafana(t)
	target := M{"refId": "A", "datasource": M{"uid": "td", "type": tdType}, "scenarioId": "random_walk"}
	dash := M{"overwrite": true, "dashboard": M{
		"uid": "rgpublic", "title": "rangeguard public", "schemaVersion": 39, "time": M{"from": "now-6h", "to": "now"},
		"panels": []M{{"id": 1, "type": "timeseries", "title": "p", "datasource": target["datasource"], "targets": []M{target},
			"gridPos": M{"h": 8, "w": 12, "x": 0, "y": 0}}},
	}}
	if code, out := call(t, "POST", grafanaURL+"/api/dashboards/db", admin, dash); code != 200 {
		t.Fatalf("create dashboard: %d %s", code, out)
	}
	code, out := call(t, "POST", grafanaURL+"/api/dashboards/uid/rgpublic/public-dashboards", admin, M{"isEnabled": true, "timeSelectionEnabled": true, "share": "public"})
	if code != 200 {
		code, out = call(t, "GET", grafanaURL+"/api/dashboards/uid/rgpublic/public-dashboards", admin, nil)
	}
	var pd struct{ AccessToken string }
	json.Unmarshal(out, &pd)
	if pd.AccessToken == "" {
		t.Fatalf("enable public dashboard: %d %s", code, out)
	}
	now := time.Now()
	path := "/api/public/dashboards/" + pd.AccessToken + "/panels/1/query"
	body := M{"intervalMs": 3600000, "maxDataPoints": 5000, "timeRange": M{"from": ms(now.Add(-30 * day)), "to": ms(now), "timezone": "utc"}}
	code, out = call(t, "POST", grafanaURL+path, "", body)
	first, _ := span(t, code, out)
	near(t, "public direct", first, now.Add(-30*day), 2*time.Hour)

	g := e2eGuard(t, "grafana", grafanaURL, "rules:\n  - {name: public, match: {role: [public]}, max_range: 2d}\n")
	code, out = call(t, "POST", g+path, "", body)
	first, notice := span(t, code, out)
	near(t, "public guarded", first, now.Add(-2*day), 2*time.Hour)
	if !strings.Contains(notice, "shortened to 2d") {
		t.Errorf("public notice: %q", notice)
	}
	rel := M{"intervalMs": 3600000, "maxDataPoints": 5000, "timeRange": M{"from": "now-30d", "to": "now", "timezone": "utc"}}
	code, out = call(t, "POST", g+path, "", rel)
	first, _ = span(t, code, out)
	near(t, "public relative", first, now.Add(-2*day), 2*time.Hour)
}

// promSpan returns the first timestamp of a Prometheus or Loki matrix
// response and its warnings.
func promSpan(t *testing.T, code int, out []byte) (time.Time, string) {
	t.Helper()
	var r struct {
		Status   string
		Warnings []string
		Data     struct {
			Result []struct{ Values [][]json.RawMessage }
		}
	}
	if err := json.Unmarshal(out, &r); err != nil || code != 200 || r.Status != "success" || len(r.Data.Result) == 0 || len(r.Data.Result[0].Values) == 0 {
		t.Fatalf("API query failed: %d %.600s", code, out)
	}
	f, _ := strconv.ParseFloat(string(r.Data.Result[0].Values[0][0]), 64)
	return time.UnixMilli(int64(f * 1000)), strings.Join(r.Warnings, "; ")
}

func TestE2EPrometheusMode(t *testing.T) {
	e2e(t)
	now := time.Now()
	raw := fmt.Sprintf("/api/v1/query_range?query=time()&start=%d&end=%d&step=600", now.Add(-10*day).Unix(), now.Unix())
	code, out := call(t, "GET", promURL+raw, "", nil)
	first, _ := promSpan(t, code, out)
	near(t, "direct", first, now.Add(-10*day), 15*time.Minute)

	g := e2eGuard(t, "prometheus", promURL, "defaults: {max_range: 2h, max_lookback: 30d}\n")
	code, out = call(t, "GET", g+raw, "", nil)
	first, warn := promSpan(t, code, out)
	near(t, "GET", first, now.Add(-2*time.Hour), 15*time.Minute)
	if !strings.Contains(warn, "shortened to 2h") {
		t.Errorf("warning: %q", warn)
	}
	form := url.Values{"query": {"time()"}, "start": {now.Add(-10 * day).Format(time.RFC3339)}, "end": {now.Format(time.RFC3339)}, "step": {"600"}}
	code, out = call(t, "POST", g+"/api/v1/query_range", "", form)
	first, _ = promSpan(t, code, out)
	near(t, "POST", first, now.Add(-2*time.Hour), 15*time.Minute)

	code, out = call(t, "GET", g+"/api/v1/query?query="+url.QueryEscape("count_over_time(up[90d])"), "", nil)
	if code != 400 || !strings.Contains(string(out), "bad_data") {
		t.Errorf("instant with a long selector: %d %s", code, out)
	}
	code, out = call(t, "GET", g+"/api/v1/query?query="+url.QueryEscape("count_over_time(up[5m])"), "", nil)
	if code != 200 || !strings.Contains(string(out), `"success"`) {
		t.Errorf("short instant: %d %s", code, out)
	}
	for _, p := range []string{"/api/v1/labels", "/api/v1/label/__name__/values", "/api/v1/series?match[]=up", "/api/v1/status/buildinfo"} {
		if code, out := call(t, "GET", g+p, "", nil); code != 200 || !strings.Contains(string(out), `"success"`) {
			t.Errorf("%s: %d %.200s", p, code, out)
		}
	}
	g = e2eGuard(t, "prometheus", promURL, "defaults: {max_range: 2h, action: reject}\n")
	if code, out := call(t, "GET", g+raw, "", nil); code != 400 || !strings.Contains(string(out), "the limit is 2h") {
		t.Errorf("reject: %d %s", code, out)
	}
}

func TestE2ELokiMode(t *testing.T) {
	e2e(t)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		resp, err := http.Get(lokiURL + "/ready")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Loki at %s did not become ready", lokiURL)
		}
		time.Sleep(3 * time.Second)
	}
	now := time.Now()
	raw := fmt.Sprintf("/loki/api/v1/query_range?query=%s&start=%d&end=%d&step=600", url.QueryEscape("vector(1)"), now.Add(-5*day).UnixNano(), now.UnixNano())
	code, out := call(t, "GET", lokiURL+raw, "", nil)
	first, _ := promSpan(t, code, out)
	near(t, "direct", first, now.Add(-5*day), 15*time.Minute)

	g := e2eGuard(t, "loki", lokiURL, "defaults: {max_range: 6h}\n")
	code, out = call(t, "GET", g+raw, "", nil)
	first, _ = promSpan(t, code, out)
	near(t, "guarded", first, now.Add(-6*time.Hour), 15*time.Minute)

	code, out = call(t, "GET", g+"/loki/api/v1/query_range?step=600&since=3d&query="+url.QueryEscape("vector(1)"), "", nil)
	first, _ = promSpan(t, code, out)
	near(t, "since", first, now.Add(-6*time.Hour), 15*time.Minute)

	if code, out := call(t, "GET", g+"/loki/api/v1/labels", "", nil); code != 200 {
		t.Errorf("labels: %d %s", code, out)
	}
	g = e2eGuard(t, "loki", lokiURL, "defaults: {max_range: 6h, action: reject}\n")
	if code, out := call(t, "GET", g+raw, "", nil); code != 400 || !strings.Contains(string(out), "the limit is 6h") {
		t.Errorf("reject: %d %s", code, out)
	}
}
