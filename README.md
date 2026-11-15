# grafana-rangeguard

A reverse proxy that enforces time range limits on Grafana queries. It sits in
front of Grafana, reads the time range of every query request and shortens or
rejects ranges that exceed the configured limits. Limits can depend on the data
source, the dashboard, the role of the user, the user and the organisation.
Public dashboards are covered as well.

A second mode puts the same limits in front of a Prometheus or Loki HTTP API,
where they also apply to clients other than Grafana.

```
browser --> grafana-rangeguard --> Grafana --> data sources
            POST /api/ds/query           (all data source types)
            POST /api/public/dashboards/<token>/panels/<id>/query
            /api/datasources/proxy/... and .../resources/... (Prometheus, Loki)

Grafana --> grafana-rangeguard --> Prometheus or Loki      (mode: prometheus | loki)
            /api/v1/query_range, /api/v1/query, /api/v1/series, /api/v1/labels, ...
```

## Why

Grafana has no setting that limits how far back or how wide a query may go.
The time picker accepts any range, and one viewer who selects "last 5 years"
on a busy dashboard can exhaust the memory of the database behind it or run up
the bill of a metered data source.

- [grafana/grafana#4288](https://github.com/grafana/grafana/issues/4288)
  "Data source & dashboard limits on time range": 239 reactions, 124 comments,
  open since March 2016. The comments name three cases: databases that run out
  of memory on wide ranges, public dashboards that must not expose or load the
  full history, and data sources that are billed per query. The last comment
  is from October 2026.
- Two pull requests that added "Max time range" and "Oldest From" dashboard
  settings ([#36384](https://github.com/grafana/grafana/pull/36384),
  [#47055](https://github.com/grafana/grafana/pull/47055)) were closed
  unmerged in 2022.
- The workarounds in the thread are per data source: database views with a
  date filter for MySQL, or the query limits of the backend itself. Mimir,
  Thanos and Loki have a maximum query length. Plain Prometheus, SQL data
  sources, InfluxDB and Elasticsearch do not, and none of the backend limits
  can depend on the Grafana role or dashboard.
- I found no existing tool for this on GitHub (searches for Grafana time range
  limit proxies on 2026-11-15) and none is linked in the issue.

## How it works

All panels, Explore and most API clients send their queries to
`POST /api/ds/query`. The request body carries `from` and `to` and, per query,
the data source. Grafana evaluates every data source type against that range
(for SQL data sources through `$__timeFilter` and related macros). The proxy
therefore does not need to understand the query language:

1. It finds the rule for each query in the request (first match wins) and
   takes the strictest limits when one request addresses several data sources.
2. It compares the range with `max_range` (width) and `max_lookback` (oldest
   allowed start, relative to now).
3. With `action: clamp` it rewrites `from` so that the newest part of the
   range is kept. With `action: reject` it answers with HTTP 400 and an error
   per query, without contacting Grafana.
4. For shortened requests it adds a warning notice to every returned data
   frame (`schema.meta.notices`), which is the field Grafana uses for panel
   notices. It also sets the response header `X-Rangeguard: clamped`.

Requests within the limits are forwarded byte for byte. Everything that is not
a query (pages, assets, API, WebSocket) is proxied unchanged.

Further details:

- **Prometheus and Loki expressions.** For queries to data sources of type
  `prometheus` and `loki` the proxy also reads the expression. Range selectors
  (`[30d]`), subqueries (`[30d:1h]`) and `offset` count towards the range. A
  selector that is longer than the limit cannot be fixed by shortening the
  range, so such a query is rejected. Numeric `@` modifiers older than
  `max_lookback` are rejected.
- **Data source proxy and resource paths.** Requests to
  `/api/datasources/proxy/...` and `/api/datasources/uid/<uid>/resources/...`
  that end in a Prometheus or Loki API path are limited through their `start`
  and `end` parameters.
- **Public dashboards.** The query request of a public dashboard carries only
  a time range. The proxy applies the rule that matches role `public`.
- **Who is asking.** When a rule matches on `role`, `user` or `org`, the proxy
  forwards the session cookie or Authorization header to Grafana's
  `/api/user` and `/api/user/orgs` and caches the answer for 30 seconds.
  Requests without a session are `anonymous`.
- **Data source type.** By default the type is read from the request. With
  `grafana_token` set, the proxy looks the type up in Grafana by uid, so a
  client cannot avoid a rule by sending another type.

In the `prometheus` and `loki` modes the proxy handles `query_range`, `query`,
`series`, `labels`, `label/<name>/values`, `query_exemplars` and the Loki
index and detection endpoints. It supports GET and form POST, keeps the
timestamp format of the request (seconds, RFC 3339, nanoseconds), and
understands Loki's `since`. Instant queries cannot be shortened and are
rejected when their selectors exceed the limit. Prometheus metadata requests
without `start` would cover the whole database and get a `start`. Shortened
Prometheus responses carry an entry in `warnings`. Rules in these modes can
match on the user name in the `X-Grafana-User` header, which Grafana sends
when `send_user_header` is enabled.

## Installation

```
go install github.com/Jwrede/grafana-rangeguard@latest
```

or build from a checkout with `go build .` (Go 1.22 or newer). The only
dependency is `gopkg.in/yaml.v3`.

## Example

`examples/grafana.yaml`:

```yaml
listen: ":3001"
metrics_listen: "127.0.0.1:9311"
mode: grafana
upstream: http://127.0.0.1:3000

rules:
  - name: admins
    match: {role: [Admin]}

  - name: public-dashboards
    match: {role: [public]}
    max_range: 7d

  - name: capacity-board
    match: {dashboard_uid: [capacity-planning]}
    max_range: 1y

  - name: logs
    match: {datasource_type: [loki, elasticsearch]}
    max_range: 24h
    max_lookback: 30d
    action: reject

  - name: viewers
    match: {role: [Viewer, anonymous]}
    max_range: 30d
    max_lookback: 400d

defaults:
  max_range: 90d
  action: clamp
```

```
$ grafana-rangeguard check -config examples/grafana.yaml
mode grafana, upstream http://127.0.0.1:3000, 5 rules, first match wins
 1. admins                   max_range none   max_lookback none   clamp  role=Admin
 2. public-dashboards        max_range 7d     max_lookback none   clamp  role=public
 3. capacity-board           max_range 365d   max_lookback none   clamp  dashboard_uid=capacity-planning
 4. logs                     max_range 1d     max_lookback 30d    reject datasource_type=loki|elasticsearch
 5. viewers                  max_range 30d    max_lookback 400d   clamp  role=Viewer|anonymous
    defaults                 max_range 90d    max_lookback none   clamp

$ grafana-rangeguard explain -config examples/grafana.yaml -from now-90d -to now -role Viewer -ds-type mysql
rule:      viewers (max_range 30d, max_lookback 400d)
requested: 90d
decision:  clamp to 2026-10-16T05:22:09Z .. 2026-11-15T05:22:09Z
reason:    time range shortened to 30d: the query covers 90d, the limit is 30d (rangeguard rule viewers)

$ grafana-rangeguard explain -config examples/grafana.yaml -from now-3d -to now -role Editor -ds-type loki
rule:      logs (max_range 1d, max_lookback 30d)
requested: 3d
decision:  reject
reason:    time range rejected: the query covers 3d, the limit is 1d (rangeguard rule logs)

$ grafana-rangeguard -config examples/grafana.yaml
grafana-rangeguard dev: mode grafana, listening on :3001, upstream http://127.0.0.1:3000, 5 rules
clamp endpoint=ds_query user="vera" dashboard="abc123": time range shortened to 30d: the query covers 90d, the limit is 30d (rangeguard rule viewers)
```

Users then open the proxy address instead of Grafana. If Grafana's
`root_url` names a public address, put the proxy behind the same address
(for example as the upstream of the existing reverse proxy).

For a first rollout set `dry_run: true`. The proxy then changes nothing and
logs and counts what it would have done:

```
rangeguard_requests_total{endpoint="ds_query",decision="dry_run_reject"} 1
```

`examples/prometheus.yaml` shows the mode in front of Prometheus.

### Configuration reference

| Key | Meaning |
|---|---|
| `listen` | Address of the proxy, default `:3001` |
| `metrics_listen` | Optional address for `/metrics` |
| `mode` | `grafana` (default), `prometheus` or `loki` |
| `upstream` | URL of Grafana, Prometheus or Loki |
| `grafana_token` | Optional service account token used to look up data source types. `${VAR}` is replaced from the environment |
| `dry_run` | Log and count decisions without applying them |
| `user_header` | Header with the user name in `prometheus` and `loki` mode, default `X-Grafana-User` |
| `rules[].match` | `datasource_uid`, `datasource_type`, `dashboard_uid`, `role` (`Admin`, `Editor`, `Viewer`, `None`, `anonymous`, `public`, `unknown`), `user`, `org`. All given keys must match, each key matches any of its values |
| `rules[].max_range`, `defaults.max_range` | Largest width of a query, for example `30d`. Empty means no limit |
| `rules[].max_lookback`, `defaults.max_lookback` | Oldest allowed start, as a distance from now |
| `rules[].action`, `defaults.action` | `clamp` (default) or `reject` |

Durations use the units `ms`, `s`, `m`, `h`, `d`, `w` and `y`.

## Tests

- Unit tests run the proxy against stand-ins for Grafana, Prometheus and Loki
  and check what arrives upstream: `go test ./...`
- End-to-end tests run against real servers in Docker: `e2e/run.sh`. They use
  Grafana's built-in test data source and the Prometheus expression `time()`,
  which return one point per step over the whole requested range. Each test
  first queries the server directly and checks that the data starts at the
  requested time, then repeats the query through the proxy and checks that
  the data starts at the limit. Covered: roles (anonymous, Viewer, Editor,
  Admin) with real Grafana users, reject, lookback, relative time values, a
  Prometheus data source, the data source proxy path, type lookup with a
  service account token, a public dashboard, and the Prometheus and Loki
  modes with GET, form POST, `since`, instant queries and metadata requests.
- CI runs the end-to-end tests against the versions listed in
  `.github/workflows/ci.yml`, and again every Monday against the `latest`
  images.

## Limits

- The proxy limits queries. It does not change the time picker, so users can
  still select a wider range and then see the shortened data with a notice.
  The notice is checked in the API response; how each Grafana version renders
  it in the panel is not part of the tests.
- It only protects requests that pass through it. Clients that reach Grafana
  or the database directly are not limited.
- In `grafana` mode only Prometheus and Loki expressions are inspected. A
  user who may edit queries can write a SQL, Flux or Elasticsearch query that
  ignores the dashboard time range. The limits are reliable for users who can
  only view dashboards, and for Prometheus and Loki.
- `dashboard_uid` comes from the `X-Dashboard-Uid` header or the Referer,
  which the browser sends. It is suited for giving a dashboard a different
  limit, not as a security boundary.
- On public dashboards the request does not name the data source, so only
  rules without data source conditions apply. Requests without a time range
  use the range stored in the dashboard and pass unchanged.
- The expression check is an upper bound based on the text of the query, not
  a full parser. Offsets on nested subqueries are counted once. Durations
  given as Grafana variables are not known to the proxy in `grafana` mode. In
  the `prometheus` and `loki` modes Grafana has already replaced them.
- Relative time values that round to a unit (`now/d`) are evaluated in UTC.
- Grafana Live streaming and alert rule evaluation do not go through
  `/api/ds/query` and are not limited.
- Not tested: Grafana behind a sub path with a real Grafana (unit test only),
  Grafana Enterprise and Grafana Cloud, data sources other than the test data
  source and Prometheus through real Grafana.

## License

MIT
