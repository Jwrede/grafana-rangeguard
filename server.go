package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxBody = 16 << 20

type ctxKey struct{}

// note travels with a forwarded request and tells ModifyResponse which
// text to add to the response and in which format.
type note struct {
	text string
	kind string // "frames" (Grafana query response) or "prom" (Prometheus API)
}

type Server struct {
	cfg    *Config
	proxy  *httputil.ReverseProxy
	client *http.Client
	now    func() time.Time
	logf   func(string, ...any)

	mu       sync.Mutex
	counters map[string]int64
	idCache  map[[32]byte]idEntry
	dsType   map[string]string
	dsByID   map[string]string
	dsAt     time.Time
}

type idEntry struct {
	sub Subject
	at  time.Time
}

func NewServer(cfg *Config) *Server {
	s := &Server{
		cfg:      cfg,
		client:   &http.Client{Timeout: 10 * time.Second},
		now:      time.Now,
		logf:     log.Printf,
		counters: map[string]int64{},
		idCache:  map[[32]byte]idEntry{},
	}
	s.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(cfg.upstream)
			pr.SetXForwarded()
			if cfg.Mode == "grafana" {
				// Grafana compares Origin with Host for CSRF protection.
				pr.Out.Host = pr.In.Host
			}
		},
		ModifyResponse: s.modifyResponse,
		ErrorLog:       log.Default(),
	}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Mode == "grafana" {
		s.serveGrafana(w, r)
		return
	}
	s.serveAPI(w, r)
}

func (s *Server) count(endpoint, decision string) {
	s.mu.Lock()
	s.counters[fmt.Sprintf(`rangeguard_requests_total{endpoint=%q,decision=%q}`, endpoint, decision)]++
	s.mu.Unlock()
}

func (s *Server) metricsHandler(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	keys := make([]string, 0, len(s.counters))
	for k := range s.counters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# HELP rangeguard_requests_total Guarded query requests by endpoint and decision.\n# TYPE rangeguard_requests_total counter\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s %d\n", k, s.counters[k])
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	io.WriteString(w, b.String())
}

// record logs and counts a decision and reports what to do with it:
// "pass", "clamp" or "reject". In dry-run mode the result is always "pass".
func (s *Server) record(endpoint string, sub Subject, d Decision) string {
	out := "pass"
	if d.Reject {
		out = "reject"
	} else if d.Changed {
		out = "clamp"
	}
	if out == "pass" {
		s.count(endpoint, "pass")
		return out
	}
	who := sub.User
	if who == "" {
		who = sub.Role
	}
	if s.cfg.DryRun {
		s.count(endpoint, "dry_run_"+out)
		s.logf("dry-run %s endpoint=%s user=%q dashboard=%q: %s", out, endpoint, who, sub.Dashboard, d.Reason)
		return "pass"
	}
	s.count(endpoint, out)
	s.logf("%s endpoint=%s user=%q dashboard=%q: %s", out, endpoint, who, sub.Dashboard, d.Reason)
	return out
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	r.Body.Close()
	if err != nil {
		http.Error(w, "rangeguard: cannot read request body", http.StatusBadRequest)
		return nil, false
	}
	if len(body) > maxBody {
		http.Error(w, "rangeguard: request body too large", http.StatusRequestEntityTooLarge)
		return nil, false
	}
	return body, true
}

func setBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
}

// forward sends the request upstream. With a note, the response is read
// uncompressed so that the note can be added to it.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, n *note) {
	if n != nil {
		r.Header.Del("Accept-Encoding")
		w.Header().Set("X-Rangeguard", "clamped")
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, n))
	}
	s.proxy.ServeHTTP(w, r)
}

func (s *Server) modifyResponse(resp *http.Response) error {
	n, _ := resp.Request.Context().Value(ctxKey{}).(*note)
	if n == nil || resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "" ||
		!strings.Contains(resp.Header.Get("Content-Type"), "json") {
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}
	var out []byte
	if n.kind == "frames" {
		out = addFrameNotice(body, n.text)
	} else {
		out = addPromWarning(body, n.text)
	}
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	return nil
}

type raw = map[string]json.RawMessage

// addFrameNotice adds a warning notice to every data frame of a Grafana
// query response. Grafana shows frame notices in the panel header. On any
// unexpected structure the body is returned unchanged.
func addFrameNotice(body []byte, text string) []byte {
	var top raw
	if json.Unmarshal(body, &top) != nil {
		return body
	}
	var results map[string]raw
	if json.Unmarshal(top["results"], &results) != nil {
		return body
	}
	changed := false
	for ref, res := range results {
		var frames []raw
		if json.Unmarshal(res["frames"], &frames) != nil || len(frames) == 0 {
			continue
		}
		for i, f := range frames {
			schema := raw{}
			if len(f["schema"]) > 0 && json.Unmarshal(f["schema"], &schema) != nil {
				return body
			}
			meta := map[string]any{}
			if len(schema["meta"]) > 0 && json.Unmarshal(schema["meta"], &meta) != nil {
				return body
			}
			notices, _ := meta["notices"].([]any)
			meta["notices"] = append(notices, map[string]any{"severity": "warning", "text": text})
			schema["meta"], _ = json.Marshal(meta)
			f["schema"], _ = json.Marshal(schema)
			frames[i] = f
		}
		res["frames"], _ = json.Marshal(frames)
		results[ref] = res
		changed = true
	}
	if !changed {
		return body
	}
	top["results"], _ = json.Marshal(results)
	out, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return out
}

// addPromWarning appends a warning to a Prometheus API response.
func addPromWarning(body []byte, text string) []byte {
	var top raw
	if json.Unmarshal(body, &top) != nil || string(top["status"]) != `"success"` {
		return body
	}
	var warnings []string
	if len(top["warnings"]) > 0 && json.Unmarshal(top["warnings"], &warnings) != nil {
		return body
	}
	top["warnings"], _ = json.Marshal(append(warnings, text))
	out, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return out
}

// jstr returns a JSON string or number as text.
func jstr(v json.RawMessage) string {
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(v, &n) == nil {
		return n.String()
	}
	return ""
}

func msJSON(t time.Time) json.RawMessage {
	return json.RawMessage(strconv.Quote(strconv.FormatInt(t.UnixMilli(), 10)))
}

// identity asks Grafana who sent the request. It forwards the session
// cookie or Authorization header to /api/user and /api/user/orgs and
// caches the answer for 30 seconds.
func (s *Server) identity(r *http.Request) (role, user string, org int64) {
	key := sha256.Sum256([]byte(r.Header.Get("Cookie") + "\x00" + r.Header.Get("Authorization") + "\x00" + r.Header.Get("X-Grafana-Org-Id")))
	s.mu.Lock()
	e, ok := s.idCache[key]
	s.mu.Unlock()
	if ok && s.now().Sub(e.at) < 30*time.Second {
		return e.sub.Role, e.sub.User, e.sub.Org
	}
	sub := Subject{Role: "unknown"}
	var u struct {
		Login string `json:"login"`
		OrgID int64  `json:"orgId"`
	}
	switch code := s.grafanaGet(r, "/api/user", &u); {
	case code == http.StatusUnauthorized:
		sub.Role = "anonymous"
	case code == http.StatusOK:
		sub.User, sub.Org = u.Login, u.OrgID
		var orgs []struct {
			OrgID int64  `json:"orgId"`
			Role  string `json:"role"`
		}
		if s.grafanaGet(r, "/api/user/orgs", &orgs) == http.StatusOK {
			for _, o := range orgs {
				if o.OrgID == u.OrgID {
					sub.Role = o.Role
				}
			}
		}
	default:
		s.count("identity", "error")
	}
	s.mu.Lock()
	if len(s.idCache) > 10000 {
		s.idCache = map[[32]byte]idEntry{}
	}
	s.idCache[key] = idEntry{sub, s.now()}
	s.mu.Unlock()
	return sub.Role, sub.User, sub.Org
}

func (s *Server) grafanaGet(r *http.Request, path string, out any) int {
	req, err := http.NewRequestWithContext(r.Context(), "GET", strings.TrimRight(s.cfg.upstream.String(), "/")+path, nil)
	if err != nil {
		return 0
	}
	if r != nil {
		for _, h := range []string{"Cookie", "Authorization", "X-Grafana-Org-Id"} {
			if v := r.Header.Get(h); v != "" {
				req.Header.Set(h, v)
			}
		}
	}
	return s.doJSON(req, out)
}

func (s *Server) doJSON(req *http.Request, out any) int {
	resp, err := s.client.Do(req)
	if err != nil {
		s.logf("request to %s failed: %v", req.URL.Path, err)
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil {
			return 0
		}
	}
	return resp.StatusCode
}

// resolveDS fills in the data source type from Grafana when a token is
// configured. Without a token the type from the request body is used.
func (s *Server) resolveDS(ctx context.Context, ds DS, id string) DS {
	if s.cfg.GrafanaToken == "" {
		return ds
	}
	s.mu.Lock()
	stale := s.now().Sub(s.dsAt) > time.Minute
	s.mu.Unlock()
	if stale {
		req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(s.cfg.upstream.String(), "/")+"/api/datasources", nil)
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+s.cfg.GrafanaToken)
			var list []struct {
				ID   int64  `json:"id"`
				UID  string `json:"uid"`
				Type string `json:"type"`
			}
			if s.doJSON(req, &list) == http.StatusOK {
				types, ids := map[string]string{}, map[string]string{}
				for _, d := range list {
					types[d.UID] = d.Type
					ids[strconv.FormatInt(d.ID, 10)] = d.UID
				}
				s.mu.Lock()
				s.dsType, s.dsByID, s.dsAt = types, ids, s.now()
				s.mu.Unlock()
			} else {
				s.count("datasources", "error")
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != "" {
		if uid, ok := s.dsByID[id]; ok {
			ds.UID, ds.Known = uid, true
		}
	}
	if t, ok := s.dsType[ds.UID]; ok && ds.Known {
		ds.Type = t
	}
	return ds
}
