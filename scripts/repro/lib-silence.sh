# shellcheck shell=bash
# Silence assertions for the NetworkDoctor fault-reproduction scripts.
#
# lib.sh judges the positive direction: an expected rule fired and its
# investigation finished. This file adds the negative direction: rules that
# must NOT react during a window. Source it after lib.sh.
#
# A rule counts as silent when it produced no incident since <since> and,
# when Prometheus is reachable, has no firing ALERTS series right now.
# A pending-only alert is reported as NEAR (the expr went true but the guard
# or the `for` held) — it does not fail the assertion, but it belongs in the
# run notes: the margin was thinner than expected.
#
# Extra settings:
#   ND_PROM_URL  Prometheus base URL for the alert-level check
#                (default http://127.0.0.1:30090, the lab NodePort;
#                 set to "" to check incidents only)

ND_PROM_URL=${ND_PROM_URL-http://127.0.0.1:30090}

# nd_assert_silent <since> "<rule ids>"
# One line per rule: SILENT / NEAR (pending only) / VIOLATION (incident or
# firing alert). Returns non-zero when any rule has a VIOLATION.
nd_assert_silent() {
  local since=$1 rules=$2 rc=0 rule hits states
  echo
  echo "== silence check (since $since)"
  for rule in $rules; do
    hits=$(curl -fsS "http://127.0.0.1:$ND_PF_PORT/incidents" | python3 -c '
import json, sys
since, rule = sys.argv[1], sys.argv[2]
for i in json.load(sys.stdin)["incidents"]:
    if i["rule_id"] == rule and i["starts_at"] >= since:
        print(i["incident_id"], i["alert_status"])
' "$since" "$rule") || nd_die "could not list incidents (backend unreachable?)"

    states=""
    if [[ -n "$ND_PROM_URL" ]]; then
      states=$(curl -fsS "$ND_PROM_URL/api/v1/query" \
        --data-urlencode "query=ALERTS{source=\"networkdoctor\",rule_id=\"$rule\",alertstate=~\"pending|firing\"}" \
        2>/dev/null | python3 -c '
import json, sys
try:
    r = json.load(sys.stdin)["data"]["result"]
    print(" ".join(sorted({s["metric"]["alertstate"] for s in r})))
except Exception:
    pass')
    fi

    if [[ -n "$hits" || "$states" == *firing* ]]; then
      rc=1
      printf '  %-7s VIOLATION  %s%s\n' "$rule" \
        "${hits:+incident: $hits }" "${states:+alerts: $states}"
    elif [[ "$states" == *pending* ]]; then
      printf '  %-7s NEAR       pending only — expr went true, guard/for held\n' "$rule"
    else
      printf '  %-7s SILENT\n' "$rule"
    fi
  done
  return "$rc"
}
