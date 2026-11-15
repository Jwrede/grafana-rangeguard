package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var apiPath = regexp.MustCompile(`/(loki/)?api/v1/(query_range|query|series|labels|label/[^/]+/values|query_exemplars|index/stats|index/volume|index/volume_range|detected_fields|detected_labels|patterns)$`)

// classifyAPI recognises Prometheus and Loki HTTP API paths that take a
// time range. kind is "range", "instant" or "meta".
func classifyAPI(path string) (kind string, loki bool, ok bool) {
	m := apiPath.FindStringSubmatch(path)
	if m == nil {
		return "", false, false
	}
	switch m[2] {
	case "query_range", "index/volume_range", "query_exemplars":
		kind = "range"
	case "query":
		kind = "instant"
	default:
		kind = "meta"
	}
	return kind, m[1] != "", true
}

// serveAPI is the handler of the prometheus and loki modes.
func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	kind, loki, ok := classifyAPI(r.URL.Path)
	if !ok {
		s.proxy.ServeHTTP(w, r)
		return
	}
	sub := Subject{User: r.Header.Get(s.cfg.UserHeader)}
	s.guardAPI(w, r, kind, kind, loki || s.cfg.Mode == "loki", sub, s.cfg.limitsFor(sub, DS{}))
}

// params gives access to API parameters in the URL and in a form body.
type params struct {
	q, form url.Values
	hasForm bool
	dirty   bool
}

func (p *params) get(k string) (string, bool) {
	if p.hasForm && p.form.Has(k) {
		return p.form.Get(k), true
	}
	if p.q.Has(k) {
		return p.q.Get(k), true
	}
	return "", false
}

func (p *params) set(k, v string) {
	p.dirty = true
	if p.hasForm && (p.form.Has(k) || !p.q.Has(k)) {
		p.form.Set(k, v)
		return
	}
	p.q.Set(k, v)
}

func (p *params) del(k string) {
	if p.q.Has(k) || p.form.Has(k) {
		p.dirty = true
	}
	p.q.Del(k)
	p.form.Del(k)
}

func rejectAPI(w http.ResponseWriter, loki bool, reason string) {
	w.Header().Set("X-Rangeguard", "rejected")
	if loki {
		http.Error(w, reason, http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]string{"status": "error", "errorType": "bad_data", "error": reason})
}

func (s *Server) guardAPI(w http.ResponseWriter, r *http.Request, endpoint, kind string, loki bool, sub Subject, lim Limits) {
	p := &params{q: r.URL.Query(), form: url.Values{}}
	var body []byte
	if r.Method == http.MethodPost && strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		var ok bool
		if body, ok = readBody(w, r); !ok {
			return
		}
		setBody(r, body)
		if f, err := url.ParseQuery(string(body)); err == nil {
			p.form, p.hasForm = f, true
		}
	}
	pass := func() { s.proxy.ServeHTTP(w, r) }
	if lim.unlimited() {
		s.count(endpoint, "pass")
		pass()
		return
	}
	now := s.now()
	var extra time.Duration
	if kind != "meta" {
		expr, _ := p.get("query")
		var ats []time.Time
		extra, ats = exprReach(expr)
		for _, at := range ats {
			if lim.MaxLookback > 0 && now.Sub(at) > lim.MaxLookback {
				d := Decision{Reject: true, Reason: "time range rejected: an @ modifier reads data from " + fmtDur(now.Sub(at)) + " ago, the limit is " + fmtDur(lim.MaxLookback) + " (rangeguard rule " + strings.Join(lim.Rules, ",") + ")"}
				if s.record(endpoint, sub, d) == "reject" {
					rejectAPI(w, loki, d.Reason)
					return
				}
			}
		}
	}
	from, to := now, now
	startKind := "sec"
	if loki {
		startKind = "ns"
	}
	if kind == "instant" {
		if v, ok := p.get("time"); ok {
			t, _, err := apiTime(v)
			if err != nil {
				pass() // the upstream reports the invalid parameter
				return
			}
			from, to = t, t
		}
	} else {
		if v, ok := p.get("end"); ok {
			t, _, err := apiTime(v)
			if err != nil {
				pass()
				return
			}
			to = t
		}
		v, ok := p.get("start")
		since, hasSince := p.get("since")
		switch {
		case ok:
			t, k, err := apiTime(v)
			if err != nil {
				pass()
				return
			}
			from, startKind = t, k
		case loki && hasSince:
			d, err := parseDur(since)
			if err != nil {
				pass()
				return
			}
			from = to.Add(-d)
		case loki && kind == "range":
			from = to.Add(-time.Hour) // Loki default
		case loki:
			pass() // Loki applies its own short default for metadata requests
			s.count(endpoint, "pass")
			return
		case kind == "range":
			pass() // Prometheus requires start, it reports the error
			return
		default:
			from = time.Unix(0, 0) // Prometheus metadata requests without start cover everything
		}
	}
	if kind == "meta" {
		lim.Reject = false // label and series lookups are shortened, never rejected
	}
	d := decide(lim, from, to, now, extra)
	switch s.record(endpoint, sub, d) {
	case "reject":
		rejectAPI(w, loki, d.Reason)
	case "clamp":
		p.set("start", fmtAPITime(d.From, startKind))
		p.del("since")
		r.URL.RawQuery = p.q.Encode()
		if p.hasForm {
			setBody(r, []byte(p.form.Encode()))
		}
		var n *note
		if !loki && kind != "meta" {
			n = &note{d.Reason, "prom"}
		} else {
			w.Header().Set("X-Rangeguard", "clamped")
		}
		s.forward(w, r, n)
	default:
		pass()
	}
}
