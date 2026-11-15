package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var (
	publicQueryPath = regexp.MustCompile(`/api/public/dashboards/[^/]+/panels/[^/]+/query$`)
	dsProxyPath     = regexp.MustCompile(`/api/datasources/(?:proxy/(?:uid/([^/]+)|(\d+))|uid/([^/]+)/resources|(\d+)/resources)/(.+)$`)
	dashboardRef    = regexp.MustCompile(`/d(?:-solo)?/([^/?#]+)`)
)

func (s *Server) serveGrafana(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/api/ds/query"):
		s.handleDSQuery(w, r)
	case r.Method == http.MethodPost && publicQueryPath.MatchString(p):
		s.handlePublicQuery(w, r)
	default:
		if m := dsProxyPath.FindStringSubmatch(p); m != nil {
			if kind, loki, ok := classifyAPI("/" + m[5]); ok {
				ds := DS{UID: m[1] + m[3], Known: m[1]+m[3] != ""}
				ds = s.resolveDS(r.Context(), ds, m[2]+m[4])
				sub := s.subject(r)
				s.guardAPI(w, r, "datasource_proxy", kind, loki, sub, s.cfg.limitsFor(sub, ds))
				return
			}
		}
		s.proxy.ServeHTTP(w, r)
	}
}

func (s *Server) subject(r *http.Request) Subject {
	sub := Subject{Dashboard: r.Header.Get("X-Dashboard-Uid")}
	if sub.Dashboard == "" {
		if m := dashboardRef.FindStringSubmatch(r.Header.Get("Referer")); m != nil {
			sub.Dashboard = m[1]
		}
	}
	if s.cfg.needsIdentity {
		sub.Role, sub.User, sub.Org = s.identity(r)
	}
	return sub
}

func rejectQuery(w http.ResponseWriter, reason string, refs []string) {
	results := map[string]any{}
	for _, ref := range refs {
		results[ref] = map[string]any{"error": reason, "errorSource": "downstream", "status": 400}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Rangeguard", "rejected")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]any{"message": reason, "results": results})
}

// handleDSQuery guards POST /api/ds/query, the endpoint every panel,
// Explore and most API clients use for all data source types.
func (s *Server) handleDSQuery(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var top raw
	var queries []raw
	if json.Unmarshal(body, &top) != nil || json.Unmarshal(top["queries"], &queries) != nil {
		setBody(r, body)
		s.proxy.ServeHTTP(w, r) // let Grafana report the malformed request
		return
	}
	now := s.now()
	sub := s.subject(r)
	var lim Limits
	var extra time.Duration
	var refs []string
	var ats []time.Time
	first := true
	for _, q := range queries {
		refs = append(refs, jstr(q["refId"]))
		var d struct {
			UID  string `json:"uid"`
			Type string `json:"type"`
		}
		json.Unmarshal(q["datasource"], &d)
		if d.UID == "__expr__" || d.Type == "__expr__" || d.UID == "-100" {
			continue // server side expressions read other queries, not a data source
		}
		ds := s.resolveDS(r.Context(), DS{UID: d.UID, Type: d.Type, Known: d.UID != ""}, "")
		l := s.cfg.limitsFor(sub, ds)
		if first {
			lim, first = l, false
		} else {
			lim = merge(lim, l)
		}
		if ds.Type == "prometheus" || ds.Type == "loki" {
			e, at := exprReach(jstr(q["expr"]))
			if e > extra {
				extra = e
			}
			ats = append(ats, at...)
		}
	}
	if first || lim.unlimited() {
		s.count("ds_query", "pass")
		setBody(r, body)
		s.proxy.ServeHTTP(w, r)
		return
	}
	for _, at := range ats {
		if lim.MaxLookback > 0 && now.Sub(at) > lim.MaxLookback {
			d := Decision{Reject: true, Reason: "time range rejected: an @ modifier reads data from " + fmtDur(now.Sub(at)) + " ago, the limit is " + fmtDur(lim.MaxLookback) + " (rangeguard rule " + strings.Join(lim.Rules, ",") + ")"}
			if s.record("ds_query", sub, d) == "reject" {
				rejectQuery(w, d.Reason, refs)
				return
			}
		}
	}
	// The request level range, then per query overrides.
	changed, reason := false, ""
	apply := func(obj raw, endpoint string) (stop bool) {
		if len(obj["from"]) == 0 && len(obj["to"]) == 0 {
			return false
		}
		from, errF := grafanaTime(jstr(obj["from"]), now, false)
		to, errT := grafanaTime(jstr(obj["to"]), now, true)
		d := decide(lim, from, to, now, extra)
		if errF != nil || errT != nil {
			d = Decision{Reject: true, Reason: "time range rejected: rangeguard cannot read the time range of this request"}
		}
		switch s.record(endpoint, sub, d) {
		case "reject":
			rejectQuery(w, d.Reason, refs)
			return true
		case "clamp":
			obj["from"], obj["to"] = msJSON(d.From), msJSON(d.To)
			changed, reason = true, d.Reason
		}
		return false
	}
	if apply(top, "ds_query") {
		return
	}
	for i, q := range queries {
		var tr raw
		if len(q["timeRange"]) == 0 || json.Unmarshal(q["timeRange"], &tr) != nil {
			continue
		}
		before := changed
		changed = false
		if apply(tr, "ds_query") {
			return
		}
		if changed {
			q["timeRange"], _ = json.Marshal(tr)
			queries[i] = q
		}
		changed = changed || before
	}
	if !changed {
		setBody(r, body)
		s.proxy.ServeHTTP(w, r)
		return
	}
	top["queries"], _ = json.Marshal(queries)
	out, _ := json.Marshal(top)
	setBody(r, out)
	s.forward(w, r, &note{reason, "frames"})
}

// handlePublicQuery guards the query endpoint of public (shared) dashboards.
// The request carries only a time range. The queries and data sources are
// stored in Grafana, so only role "public" and dashboard independent rules apply.
func (s *Server) handlePublicQuery(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var top, tr raw
	if json.Unmarshal(body, &top) != nil || len(top["timeRange"]) == 0 || json.Unmarshal(top["timeRange"], &tr) != nil ||
		jstr(tr["from"]) == "" || jstr(tr["to"]) == "" {
		setBody(r, body)
		s.count("public_query", "no_range")
		s.proxy.ServeHTTP(w, r)
		return
	}
	now := s.now()
	sub := Subject{Role: "public"}
	lim := s.cfg.limitsFor(sub, DS{})
	from, errF := grafanaTime(jstr(tr["from"]), now, false)
	to, errT := grafanaTime(jstr(tr["to"]), now, true)
	d := decide(lim, from, to, now, 0)
	if (errF != nil || errT != nil) && !lim.unlimited() {
		d = Decision{Reject: true, Reason: "time range rejected: rangeguard cannot read the time range of this request"}
	}
	switch s.record("public_query", sub, d) {
	case "reject":
		rejectQuery(w, d.Reason, nil)
	case "clamp":
		tr["from"], tr["to"] = msJSON(d.From), msJSON(d.To)
		top["timeRange"], _ = json.Marshal(tr)
		out, _ := json.Marshal(top)
		setBody(r, out)
		s.forward(w, r, &note{d.Reason, "frames"})
	default:
		setBody(r, body)
		s.proxy.ServeHTTP(w, r)
	}
}
