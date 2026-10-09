#!/usr/bin/env bash
# Read-only view of the NetworkDoctor backend.
#
#   incidents.sh                       list incidents from the last hour
#   incidents.sh <since RFC3339>       list incidents since a time
#   incidents.sh check <since> "<rule ids>"   PASS/WAIT/FAIL table, like the repro scripts
#   incidents.sh report <incident id> print the Markdown report
set -uo pipefail
. "$(dirname "$0")/lib.sh"

trap nd_cleanup EXIT
nd_backend_connect
BASE="http://127.0.0.1:$ND_PF_PORT"

case "${1:-}" in
  report)
    curl -fsS "$BASE/incidents/${2:?incident id}/report.md"
    ;;
  check)
    nd_status "${2:?since}" "${3:?rule ids}" --final
    ;;
  *)
    SINCE=${1:-$(date -u -d '-1 hour' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -v-1H +%Y-%m-%dT%H:%M:%SZ)}
    curl -fsS "$BASE/incidents" | python3 -c "
import json, sys
rows = [i for i in json.load(sys.stdin)['incidents'] if i['starts_at'] >= '$SINCE']
for i in sorted(rows, key=lambda i: i['starts_at']):
    print(f\"{i['starts_at']}  {i['incident_id']}  {i['rule_id']:8s} {i.get('alert_status','-'):8s} \"
          f\"holmes={i.get('holmes_status') or '-':8s} {i.get('scenario','')}\")
print(f'{len(rows)} incidents since $SINCE')"
    ;;
esac
