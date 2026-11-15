#!/usr/bin/env bash
# Starts Grafana, Prometheus and Loki in Docker (host network) and runs the
# end-to-end tests against them. Images can be chosen with GRAFANA_IMAGE,
# PROM_IMAGE and LOKI_IMAGE.
set -euo pipefail
cd "$(dirname "$0")/.."
GRAFANA_IMAGE=${GRAFANA_IMAGE:-grafana/grafana:latest}
PROM_IMAGE=${PROM_IMAGE:-prom/prometheus:latest}
LOKI_IMAGE=${LOKI_IMAGE:-grafana/loki:latest}
cleanup() { docker rm -f rg-grafana rg-prom rg-loki >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup
docker run -d --name rg-prom --network host "$PROM_IMAGE" >/dev/null
docker run -d --name rg-loki --network host "$LOKI_IMAGE" -config.file=/etc/loki/local-config.yaml >/dev/null
docker run -d --name rg-grafana --network host \
  -e GF_SECURITY_ADMIN_PASSWORD=admin \
  -e GF_AUTH_ANONYMOUS_ENABLED=true -e GF_AUTH_ANONYMOUS_ORG_ROLE=Viewer \
  -e GF_AUTH_BASIC_ENABLED=true \
  -e GF_FEATURE_TOGGLES_ENABLE=publicDashboards \
  "$GRAFANA_IMAGE" >/dev/null
if ! RG_E2E=1 go test -count=1 -run E2E -v . ; then
  for c in rg-grafana rg-prom rg-loki; do echo "--- $c"; docker logs --tail 40 "$c" 2>&1 || true; done
  exit 1
fi
