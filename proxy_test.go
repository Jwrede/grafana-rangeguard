package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fake is a stand-in for Grafana, Prometheus and Loki. It records what
// arrives and answers identity and data source lookups.
type fake struct {
	mu        sync.Mutex
	reqs      []*http.Request
	bodies    []string
	userCalls int
	srv       *httptest.Server
}

func newFake(t *testing.T) *fake {
	f := &fake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/user":
			f.mu.Lock()
			f.userCalls++
			f.mu.Unlock()
			u, _, ok := r.BasicAuth()
			if !ok {
				w.WriteHeader(401)
				return
			}
			fmt.Fprintf(w, `{"login":%q,"orgId":1}`, u)
			return
		case r.URL.Path == "/api/user/orgs":
			u, _, _ := r.BasicAuth()
			role := map[string]string{"admin": "Admin", "ed": "Editor"}[u]
			if role == "" {
				role = "Viewer"
			}
			fmt.Fprintf(w, `[{"orgId":2,"role":"Admin"},{"orgId":1,"role":%q}]`, role)
			return
		case r.URL.Path == "/api/datasources":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(401)
				return
			}
			io.WriteString(w, `[{"id":7,"uid":"prom1","type":"prometheus"},{"id":8,"uid":"logs","type":"loki"}]`)
			return
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, r)
		f.bodies = append(f.bodies, string(body))
		f.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/query") && strings.Contains(r.URL.Path, "/api/public/"), strings.HasSuffix(r.URL.Path, "/api/ds/query"):
			io.WriteString(w, `{"results":{"A":{"status":200,"frames":[{"schema":{"refId":"A","meta":{"typeVersion":[0,0]},"fields":[]},"data":{"values":[[1,2],[1.5,2.5]]}}]},"B":{"status":200,"frames":[]}}}`)
		default:
			io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) last(t *testing.T) (*http.Request, string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		t.Fatal("upstream received no request")
	}
	return f.reqs[len(f.reqs)-1], f.bodies[len(f.bodies)-1]
}

func (f *fake) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func guard(t *testing.T, f *fake, cfg string) (*Server, string) {
	t.Helper()
	c, err := parseConfig([]byte("upstream: " + f.srv.URL + "\n" + cfg))
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(c)
	s.now = func() time.Time { return t0 }
	s.logf = t.Logf
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, ts.URL
}

func do(t *testing.T, method, u, body string, hdr ...string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(method, u, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "basic" {
			req.SetBasicAuth(hdr[i+1], "pw")
		} else {
			req.Header.Set(hdr[i], hdr[i+1])
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out), resp.Header
}

func ms(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }

func dsQuery(from, to string, queries ...string) string {
	return fmt.Sprintf(`{"queries":[%s],"from":%q,"to":%q}`, strings.Join(queries, ","), from, to)
}

func q(ref, uid, typ, rest string) string {
	return fmt.Sprintf(`{"refId":%q,"datasource":{"uid":%q,"type":%q},"intervalMs":60000,"maxDataPoints":500%s}`, ref, uid, typ, rest)
}

func sentRange(t *testing.T, body string) (time.Time, time.Time) {
	t.Helper()
	var top struct{ From, To string }
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		t.Fatal(err)
	}
	a, _ := strconv.ParseInt(top.From, 10, 64)
	b, _ := strconv.ParseInt(top.To, 10, 64)
	return time.UnixMilli(a), time.UnixMilli(b)
}

const grafanaRules = `
rules:
  - name: admins
    match: {role: [Admin]}
  - name: board
    match: {dashboard_uid: [wide]}
    max_range: 60d
  - name: logs
    match: {datasource_type: [loki]}
    max_range: 24h
    action: reject
  - name: viewers
    match: {role: [Viewer, anonymous]}
    max_range: 7d
    max_lookback: 90d
defaults: {max_range: 30d}
`

func TestDSQueryClamp(t *testing.T) {
	f := newFake(t)
	_, u := guard(t, f, grafanaRules)
	body := dsQuery(ms(t0.Add(-90*day)), ms(t0), q("A", "db", "mysql", ""))
	code, out, hdr := do(t, "POST", u+"/api/ds/query", body)
	if code != 200 || hdr.Get("X-Rangeguard") != "clamped" {
		t.Fatalf("code %d, header %q: %s", code, hdr.Get("X-Rangeguard"), out)
	}
	_, got := f.last(t)
	from, to := sentRange(t, got)
	if !from.Equal(t0.Add(-7*day)) || !to.Equal(t0) {
		t.Errorf("upstream got %v .. %v", from, to)
	}
	if !strings.Contains(got, `"maxDataPoints":500`) || !strings.Contains(got, `"uid":"db"`) {
		t.Errorf("query was altered: %s", got)
	}
	var resp struct {
		Results map[string]struct {
			Frames []struct {
				Schema struct {
					Meta struct {
						TypeVersion []int `json:"typeVersion"`
						Notices     []struct{ Severity, Text string }
					}
				}
				Data struct{ Values [][]float64 }
			}
		}
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	fr := resp.Results["A"].Frames[0]
	if len(fr.Schema.Meta.Notices) != 1 || fr.Schema.Meta.Notices[0].Severity != "warning" || !strings.Contains(fr.Schema.Meta.Notices[0].Text, "shortened to 7d") {
		t.Errorf("notice missing: %s", out)
	}
	if len(fr.Schema.Meta.TypeVersion) != 2 || fr.Data.Values[1][1] != 2.5 {
		t.Errorf("frame content changed: %s", out)
	}
}

func TestDSQueryPassIsByteIdentical(t *testing.T) {
	f := newFake(t)
	_, u := guard(t, f, grafanaRules)
	body := dsQuery(ms(t0.Add(-day)), ms(t0), q("A", "db", "mysql", `,  "rawSql":"select 1"`))
	code, out, hdr := do(t, "POST", u+"/api/ds/query", body)
	_, got := f.last(t)
	if code != 200 || got != body || hdr.Get("X-Rangeguard") != "" || strings.Contains(out, "notices") {
		t.Errorf("code %d\nsent %s\ngot  %s", code, body, got)
	}
}

func TestDSQueryRoles(t *testing.T) {
	f := newFake(t)
	_, u := guard(t, f, grafanaRules)
	body := dsQuery("now-200d", "now", q("A", "db", "mysql", ""))
	// Admin: exempt.
	do(t, "POST", u+"/api/ds/query", body, "basic", "admin")
	if _, got := f.last(t); got != body {
		t.Errorf("admin request was changed: %s", got)
	}
	// Editor: defaults (30d).
	do(t, "POST", u+"/api/ds/query", body, "basic", "ed")
	_, got := f.last(t)
	if from, to := sentRange(t, got); to.Sub(from) != 30*day {
		t.Errorf("editor range %v", to.Sub(from))
	}
	// Viewer: 7d, relative times resolved against the proxy clock.
	do(t, "POST", u+"/api/ds/query", body, "basic", "vera")
	_, got = f.last(t)
	if from, to := sentRange(t, got); to.Sub(from) != 7*day || !to.Equal(t0) {
		t.Errorf("viewer range %v .. %v", from, to)
	}
	// The identity is cached per credential.
	before := f.userCalls
	do(t, "POST", u+"/api/ds/query", body, "basic", "vera")
	if f.userCalls != before {
		t.Errorf("identity lookup was not cached")
	}
}

func TestDSQueryReject(t *testing.T) {
	f := newFake(t)
	_, u := guard(t, f, grafanaRules)
	body := dsQuery(ms(t0.Add(-3*day)), ms(t0), q("A", "db", "mysql", ""), q("B", "logs", "loki", `,"expr":"{app=\"a\"}"`))
	code, out, hdr := do(t, "POST", u+"/api/ds/query", body)
	if code != 400 || f.n() != 0 || hdr.Get("X-Rangeguard") != "rejected" {
		t.Fatalf("code %d, upstream requests %d", code, f.n())
	}
	var resp struct {
		Message string
		Results map[string]struct{ Error string }
	}
	json.Unmarshal([]byte(out), &resp)
	if !strings.Contains(resp.Message, "covers 3d") || !strings.Contains(resp.Message, "limit is 1d") || resp.Results["B"].Error == "" || resp.Results["A"].Error == "" {
		t.Errorf("response: %s", out)
	}
	// Within the Loki limit: passes.
	code, _, _ = do(t, "POST", u+"/api/ds/query", dsQuery(ms(t0.Add(-time.Hour)), ms(t0), q("B", "logs", "loki", "")))
	if code != 200 || f.n() != 1 {
		t.Errorf("code %d, upstream requests %d", code, f.n())
	}
	// Unreadable time values are rejected when limits apply.
	code, _, _ = do(t, "POST", u+"/api/ds/query", dsQuery("last tuesday", "now", q("A", "db", "mysql", "")))
	if code != 400 {
		t.Errorf("unreadable range: code %d", code)
	}
}

func TestDSQueryDetails(t *testing.T) {
	f := newFake(t)
	_, u := guard(t, f, grafanaRules)
	long := ms(t0.Add(-80 * day))
	// Expressions are not data sources.
	body := dsQuery(long, ms(t0), `{"refId":"C","datasource":{"uid":"__expr__","type":"__expr__"},"expression":"$A"}`)
	do(t, "POST", u+"/api/ds/query", body)
	if _, got := f.last(t); got != body {
		t.Errorf("expression only request was changed")
	}
	// Dashboard rule by header and by Referer.
	body = dsQuery(long, ms(t0), q("A", "db", "mysql", ""))
	do(t, "POST", u+"/api/ds/query", body, "X-Dashboard-Uid", "wide")
	_, got := f.last(t)
	if from, to := sentRange(t, got); to.Sub(from) != 60*day {
		t.Errorf("dashboard header: %v", to.Sub(from))
	}
	do(t, "POST", u+"/api/ds/query", body, "Referer", "https://g.example/d/wide/some-title?orgId=1")
	_, got = f.last(t)
	if from, to := sentRange(t, got); to.Sub(from) != 60*day {
		t.Errorf("dashboard referer: %v", to.Sub(from))
	}
	// Prometheus range selectors count towards the range.
	body = dsQuery(ms(t0.Add(-6*day)), ms(t0), q("A", "p", "prometheus", `,"expr":"sum_over_time(x[3d])"`))
	do(t, "POST", u+"/api/ds/query", body)
	_, got = f.last(t)
	if from, to := sentRange(t, got); to.Sub(from) != 4*day {
		t.Errorf("selector: %v", to.Sub(from))
	}
	code, out, _ := do(t, "POST", u+"/api/ds/query", dsQuery(ms(t0.Add(-time.Hour)), ms(t0), q("A", "p", "prometheus", `,"expr":"sum_over_time(x[30d])"`)))
	if code != 400 || !strings.Contains(out, "cannot be shortened") {
		t.Errorf("long selector: %d %s", code, out)
	}
	code, out, _ = do(t, "POST", u+"/api/ds/query", dsQuery(ms(t0.Add(-time.Hour)), ms(t0), q("A", "p", "prometheus", `,"expr":"x @ 1600000000"`)))
	if code != 400 || !strings.Contains(out, "@ modifier") {
		t.Errorf("@ modifier: %d %s", code, out)
	}
	// Per query time ranges are limited too.
	body = dsQuery(ms(t0.Add(-time.Hour)), ms(t0), q("A", "db", "mysql", fmt.Sprintf(`,"timeRange":{"from":%q,"to":%q}`, long, ms(t0))))
	do(t, "POST", u+"/api/ds/query", body)
	_, got = f.last(t)
	var sent struct {
		Queries []struct{ TimeRange struct{ From, To string } }
	}
	json.Unmarshal([]byte(got), &sent)
	if sent.Queries[0].TimeRange.From != ms(t0.Add(-7*day)) {
		t.Errorf("per query range: %s", got)
	}
	// Grafana served from a sub path.
	do(t, "POST", u+"/grafana/api/ds/query", dsQuery(long, ms(t0), q("A", "db", "mysql", "")))
	r, got := f.last(t)
	if from, to := sentRange(t, got); to.Sub(from) != 7*day || r.URL.Path != "/grafana/api/ds/query" {
		t.Errorf("sub path: %v %s", to.Sub(from), r.URL.Path)
	}
	// Other paths are proxied untouched, with the original Host.
	do(t, "GET", u+"/api/dashboards/uid/abc?x=1", "")
	r, _ = f.last(t)
	if r.URL.RequestURI() != "/api/dashboards/uid/abc?x=1" || r.Host != strings.TrimPrefix(u, "http://") {
		t.Errorf("passthrough: %s host %s", r.URL.RequestURI(), r.Host)
	}
}

func TestStrictestOfMixedQueries(t *testing.T) {
	f := newFake(t)
	_, u := guard(t, f, "rules:\n  - {match: {datasource_uid: [a]}, max_range: 10d}\n  - {match: {datasource_uid: [b]}, max_range: 2d}\n")
	do(t, "POST", u+"/api/ds/query", dsQuery(ms(t0.Add(-50*day)), ms(t0), q("A", "a", "x", ""), q("B", "b", "x", ""), q("C", "c", "x", "")))
	_, got := f.last(t)
	if from, to := sentRange(t, got); to.Sub(from) != 2*day {
		t.Errorf("mixed: %v", to.Sub(from))
	}
}

func TestDataSourceTypeFromGrafana(t *testing.T) {
	f := newFake(t)
	_, u := guard(t, f, "grafana_token: tok\nrules:\n  - {match: {datasource_type: [loki]}, max_range: 1d}\n")
	// The client claims another type, Grafana knows it is Loki.
	do(t, "POST", u+"/api/ds/query", dsQuery(ms(t0.Add(-50*day)), ms(t0), q("A", "logs", "mysql", "")))
	_, got := f.last(t)
	if from, to := sentRange(t, got); to.Sub(from) != day {
		t.Errorf("resolved type: %v", to.Sub(from))
	}
	// Numeric id in a data source proxy path.
	do(t, "GET", fmt.Sprintf("%s/api/datasources/proxy/8/loki/api/v1/query_range?query=x&start=%d&end=%d", u, t0.Add(-9*day).UnixNano(), t0.UnixNano()), "")
	r, _ := f.last(t)
	if r.URL.Query().Get("start") != strconv.FormatInt(t0.Add(-day).UnixNano(), 10) {
		t.Errorf("proxy by id: %s", r.URL.RawQuery)
	}
}

func TestPublicDashboard(t *testing.T) {
	f := newFake(t)
	_, u := guard(t, f, "rules:\n  - {name: pub, match: {role: [public]}, max_range: 2d}\n")
	path := u + "/api/public/dashboards/abcdef/panels/1/query"
	code, out, _ := do(t, "POST", path, `{"intervalMs":1000,"maxDataPoints":100,"timeRange":{"from":"now-30d","to":"now","timezone":"utc"}}`)
	_, got := f.last(t)
	var sent struct {
		MaxDataPoints int
		TimeRange     struct{ From, To, Timezone string }
	}
	json.Unmarshal([]byte(got), &sent)
	if code != 200 || sent.TimeRange.From != ms(t0.Add(-2*day)) || sent.TimeRange.To != ms(t0) || sent.TimeRange.Timezone != "utc" || sent.MaxDataPoints != 100 {
		t.Errorf("public: %d %s", code, got)
	}
	if !strings.Contains(out, "shortened to 2d") {
		t.Errorf("public notice: %s", out)
	}
	// Without a time range in the request Grafana uses the stored one.
	body := `{"intervalMs":1000,"maxDataPoints":100}`
	do(t, "POST", path, body)
	if _, got := f.last(t); got != body {
		t.Errorf("public without range was changed: %s", got)
	}
}

func TestDataSourceProxyPaths(t *testing.T) {
	f := newFake(t)
	_, u := guard(t, f, "rules:\n  - {match: {datasource_uid: [prom1]}, max_range: 1d}\n")
	start, end := t0.Add(-10*day).Unix(), t0.Unix()
	for _, p := range []string{"/api/datasources/proxy/uid/prom1/api/v1/query_range", "/api/datasources/uid/prom1/resources/api/v1/query_range"} {
		code, out, _ := do(t, "GET", fmt.Sprintf("%s%s?query=up&start=%d&end=%d&step=60", u, p, start, end), "")
		r, _ := f.last(t)
		got, _, _ := apiTime(r.URL.Query().Get("start"))
		if code != 200 || !got.Equal(t0.Add(-day)) || r.URL.Query().Get("step") != "60" || r.URL.Path != p {
			t.Errorf("%s: %d %s", p, code, r.URL.RawQuery)
		}
		if !strings.Contains(out, `"warnings":["time range shortened to 1d`) {
			t.Errorf("%s: warning missing: %s", p, out)
		}
	}
	// Another data source and non query paths are not touched.
	raw := fmt.Sprintf("query=up&start=%d&end=%d&step=60", start, end)
	do(t, "GET", u+"/api/datasources/proxy/uid/other/api/v1/query_range?"+raw, "")
	if r, _ := f.last(t); r.URL.RawQuery != raw {
		t.Errorf("other data source: %s", r.URL.RawQuery)
	}
	do(t, "GET", u+"/api/datasources/uid/prom1/health?start=1", "")
	if r, _ := f.last(t); r.URL.RawQuery != "start=1" {
		t.Errorf("health: %s", r.URL.RawQuery)
	}
}

func TestDryRun(t *testing.T) {
	f := newFake(t)
	s, u := guard(t, f, "dry_run: true\ndefaults: {max_range: 1d, action: reject}\n")
	body := dsQuery(ms(t0.Add(-9*day)), ms(t0), q("A", "db", "mysql", ""))
	code, _, _ := do(t, "POST", u+"/api/ds/query", body)
	if _, got := f.last(t); code != 200 || got != body {
		t.Errorf("dry run changed the request: %d %s", code, got)
	}
	rec := httptest.NewRecorder()
	s.metricsHandler(rec, nil)
	if !strings.Contains(rec.Body.String(), `rangeguard_requests_total{endpoint="ds_query",decision="dry_run_reject"} 1`) {
		t.Errorf("metrics: %s", rec.Body.String())
	}
}

func promGuard(t *testing.T, f *fake, mode, rules string) string {
	_, u := guard(t, f, "mode: "+mode+"\n"+rules)
	return u
}

func TestPrometheusMode(t *testing.T) {
	f := newFake(t)
	u := promGuard(t, f, "prometheus", "rules:\n  - {name: ci, match: {user: [ci-bot]}}\ndefaults: {max_range: 2d, max_lookback: 30d}\n")
	start, end := t0.Add(-10*day), t0
	// GET query_range.
	code, out, _ := do(t, "GET", fmt.Sprintf("%s/api/v1/query_range?query=up&start=%d&end=%d&step=60", u, start.Unix(), end.Unix()), "")
	r, _ := f.last(t)
	got, _, _ := apiTime(r.URL.Query().Get("start"))
	if code != 200 || !got.Equal(t0.Add(-2*day)) || !strings.Contains(out, `"warnings":[`) {
		t.Errorf("GET: %d %s %s", code, r.URL.RawQuery, out)
	}
	// POST form, RFC 3339 times stay RFC 3339.
	form := url.Values{"query": {"rate(x[1h])"}, "start": {start.Format(time.RFC3339)}, "end": {end.Format(time.RFC3339)}, "step": {"60"}}
	do(t, "POST", u+"/api/v1/query_range", form.Encode(), "Content-Type", "application/x-www-form-urlencoded")
	_, body := f.last(t)
	sent, _ := url.ParseQuery(body)
	if sent.Get("start") != t0.Add(-2*day+time.Hour).Format(time.RFC3339Nano) || sent.Get("query") != "rate(x[1h])" {
		t.Errorf("POST: %s", body)
	}
	// Exempt user via header.
	raw := fmt.Sprintf("query=up&start=%d&end=%d&step=60", start.Unix(), end.Unix())
	do(t, "GET", u+"/api/v1/query_range?"+raw, "", "X-Grafana-User", "ci-bot")
	if r, _ := f.last(t); r.URL.RawQuery != raw {
		t.Errorf("exempt user: %s", r.URL.RawQuery)
	}
	// Instant queries cannot be shortened.
	n := f.n()
	code, out, _ = do(t, "GET", u+"/api/v1/query?query="+url.QueryEscape("count_over_time(up[90d])"), "")
	if code != 400 || f.n() != n || !strings.Contains(out, `"errorType":"bad_data"`) || !strings.Contains(out, "90d") {
		t.Errorf("instant: %d %s", code, out)
	}
	code, _, _ = do(t, "GET", u+"/api/v1/query?query="+url.QueryEscape("rate(up[5m])"), "")
	if code != 200 {
		t.Errorf("short instant: %d", code)
	}
	code, _, _ = do(t, "GET", fmt.Sprintf("%s/api/v1/query?query=up&time=%d", u, t0.Add(-40*day).Unix()), "")
	if code != 400 {
		t.Errorf("old instant: %d", code)
	}
	// Metadata requests without start would read everything.
	do(t, "GET", u+"/api/v1/label/job/values", "")
	r, _ = f.last(t)
	got, _, _ = apiTime(r.URL.Query().Get("start"))
	if !got.Equal(t0.Add(-2 * day)) {
		t.Errorf("label values: %s", r.URL.RawQuery)
	}
	// Other paths pass.
	do(t, "GET", u+"/api/v1/rules?start=1", "")
	if r, _ := f.last(t); r.URL.RawQuery != "start=1" {
		t.Errorf("rules: %s", r.URL.RawQuery)
	}
}

func TestLokiMode(t *testing.T) {
	f := newFake(t)
	u := promGuard(t, f, "loki", "defaults: {max_range: 6h}\n")
	// Nanosecond timestamps.
	do(t, "GET", fmt.Sprintf("%s/loki/api/v1/query_range?query=%s&start=%d&end=%d", u, url.QueryEscape(`{a="b"}`), t0.Add(-5*day).UnixNano(), t0.UnixNano()), "")
	r, _ := f.last(t)
	if r.URL.Query().Get("start") != strconv.FormatInt(t0.Add(-6*time.Hour).UnixNano(), 10) {
		t.Errorf("ns: %s", r.URL.RawQuery)
	}
	// since instead of start.
	do(t, "GET", u+"/loki/api/v1/query_range?query=x&since=3d", "")
	r, _ = f.last(t)
	if r.URL.Query().Has("since") || r.URL.Query().Get("start") != strconv.FormatInt(t0.Add(-6*time.Hour).UnixNano(), 10) {
		t.Errorf("since: %s", r.URL.RawQuery)
	}
	// Defaults (one hour) are within the limit.
	do(t, "GET", u+"/loki/api/v1/query_range?query=x", "")
	if r, _ := f.last(t); r.URL.RawQuery != "query=x" {
		t.Errorf("default range: %s", r.URL.RawQuery)
	}
	// Rejections are plain text, as Loki errors are.
	u = promGuard(t, f, "loki", "defaults: {max_range: 6h, action: reject}\n")
	code, out, hdr := do(t, "GET", u+"/loki/api/v1/query_range?query=x&since=3d", "")
	if code != 400 || !strings.HasPrefix(out, "time range rejected") || strings.Contains(hdr.Get("Content-Type"), "json") {
		t.Errorf("reject: %d %q", code, out)
	}
}
