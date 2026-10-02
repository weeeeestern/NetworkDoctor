# shellcheck shell=bash
# Shared helpers for the NetworkDoctor fault-reproduction scripts.
#
# Sourced by every rule*.sh script. Lab clusters only: the scripts change
# kernel settings (conntrack_max), add tc qdiscs and NetworkPolicies, and run
# privileged hostNetwork pods. Every change is registered for cleanup and
# undone by the EXIT trap, including on Ctrl+C.
#
# Settings (environment variables, all optional):
#   ND_NS            NetworkDoctor release namespace        (networkdoctor)
#   ND_DEMO_NS       namespace for repro pods / Cat Shop    (networkdoctor-demo)
#   ND_BACKEND_SVC   backend Service name                   (networkdoctor-backend)
#   ND_AGENT_DS      agent DaemonSet name                   (networkdoctor-agent)
#   ND_NODE          worker node to break                   (a worker hosting CoreDNS)
#   ND_PEER_NODE     second worker (Rule 2 frame sender)    (any other worker)
#   ND_IFACE         host NIC watched by the agent          (agent -iface arg)
#   ND_CATSHOP_URL   Cat Shop base URL                      (http://catshop.<demo ns>.svc:9898)
#   ND_TOOL_IMAGE    image for repro pods                   (nicolaka/netshoot:v0.16)
#   ND_DURATION      fault duration in seconds              (per script)
#   ND_WAIT_MIN      max minutes to wait for investigations (per script)
#   ND_PF_PORT       local port for the backend port-forward (18080)
#   ND_CONTEXT_DENY  regex of kubectl contexts to refuse    (prod)

set -uo pipefail

ND_NS=${ND_NS:-networkdoctor}
ND_DEMO_NS=${ND_DEMO_NS:-networkdoctor-demo}
ND_BACKEND_SVC=${ND_BACKEND_SVC:-networkdoctor-backend}
ND_AGENT_DS=${ND_AGENT_DS:-networkdoctor-agent}
ND_CATSHOP_URL=${ND_CATSHOP_URL:-http://catshop.${ND_DEMO_NS}.svc:9898}
ND_TOOL_IMAGE=${ND_TOOL_IMAGE:-nicolaka/netshoot:v0.16}
ND_PF_PORT=${ND_PF_PORT:-18080}
ND_CONTEXT_DENY=${ND_CONTEXT_DENY:-prod}
ND_LABEL_KEY=networkdoctor.io/repro
ND_LOGDIR=$(mktemp -d "${TMPDIR:-/tmp}/nd-repro.XXXXXX")

ND_PODS=()       # repro pods created by this run
ND_UNDO=()       # shell commands run (in reverse) on exit
ND_BG_PIDS=()    # local background processes (exec loops, port-forward)
ND_FAULT_END=0   # epoch seconds when the fault window closes (nd_fault_window)

nd_log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
nd_die() { nd_log "ERROR: $*"; exit 1; }
nd_now() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# ------------------------------------------------------------------ safety
nd_preflight() {
  command -v kubectl >/dev/null || nd_die "kubectl not found"
  command -v python3 >/dev/null || nd_die "python3 not found"
  command -v curl >/dev/null || nd_die "curl not found"
  local ctx
  ctx=$(kubectl config current-context 2>/dev/null) || nd_die "no kubectl context"
  if [[ "$ctx" =~ $ND_CONTEXT_DENY ]]; then
    nd_die "refusing to run against context '$ctx' (matches ND_CONTEXT_DENY='$ND_CONTEXT_DENY')"
  fi
  kubectl -n "$ND_NS" get svc "$ND_BACKEND_SVC" >/dev/null 2>&1 \
    || nd_die "backend Service $ND_NS/$ND_BACKEND_SVC not found"
  nd_log "context=$ctx  logs=$ND_LOGDIR"
  trap nd_cleanup EXIT
  trap 'exit 130' INT TERM
}

nd_on_exit() { ND_UNDO+=("$1"); }

# Stop the fault loops, then run the undo steps in reverse. Safe to call twice.
nd_restore() {
  local i pod
  ((${#ND_UNDO[@]})) || return 0
  # Stop loops first so none of them re-applies a change after its undo.
  for pod in "${ND_PODS[@]}"; do nd_x "$pod" 'pkill -f nd-repro-loo[p]' >/dev/null 2>&1; done
  for ((i = ${#ND_UNDO[@]} - 1; i >= 0; i--)); do
    eval "${ND_UNDO[$i]}" >/dev/null 2>&1
  done
  nd_log "fault removed (${#ND_UNDO[@]} undo steps)"
  ND_UNDO=()
}

# nd_fault_window <seconds>: the fault is removed after <seconds> even if the
# script is still waiting for investigations.
nd_fault_window() { ND_FAULT_END=$(($(date +%s) + $1)); }

nd_cleanup() {
  local rc=$?
  set +e
  nd_restore
  for pid in "${ND_BG_PIDS[@]}"; do kill "$pid" 2>/dev/null; done
  if ((${#ND_PODS[@]})); then
    kubectl -n "$ND_DEMO_NS" delete pod "${ND_PODS[@]}" --ignore-not-found --wait=false >/dev/null 2>&1
  fi
  nd_log "cleanup done (${#ND_PODS[@]} repro pods deleted)"
  exit "$rc"
}

# ------------------------------------------------------------- discovery
nd_iface() {
  if [[ -n "${ND_IFACE:-}" ]]; then echo "$ND_IFACE"; return; fi
  kubectl -n "$ND_NS" get ds "$ND_AGENT_DS" \
    -o jsonpath='{.spec.template.spec.containers[0].args}' \
    | grep -oE -- '-iface=[^",]+' | head -1 | cut -d= -f2
}

nd_workers() {
  kubectl get nodes -l '!node-role.kubernetes.io/control-plane' \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'
}

# A worker that runs a CoreDNS pod, so Rules 3/4/5 can share one node.
nd_node() {
  if [[ -n "${ND_NODE:-}" ]]; then echo "$ND_NODE"; return; fi
  local workers n
  workers=$(nd_workers)
  for n in $(kubectl -n kube-system get pod -l k8s-app=kube-dns \
      -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}'); do
    grep -qx "$n" <<<"$workers" && { echo "$n"; return; }
  done
  head -1 <<<"$workers"
}

nd_peer_node() {
  if [[ -n "${ND_PEER_NODE:-}" ]]; then echo "$ND_PEER_NODE"; return; fi
  nd_workers | grep -vx "$1" | head -1
}

# ------------------------------------------------------------ repro pods
# nd_pod <name> <node> host|pod
#   host: hostNetwork + hostPID + privileged (node-level faults)
#   pod : pod network with NET_ADMIN (tc netem on its own eth0)
nd_pod() {
  local name=$1 node=$2 mode=$3 spec sc
  if [[ "$mode" == host ]]; then
    spec='hostNetwork: true
  hostPID: true
  dnsPolicy: ClusterFirstWithHostNet'
    sc='privileged: true'
  else
    spec='hostNetwork: false'
    sc='capabilities: {add: ["NET_ADMIN"]}'
  fi
  ND_PODS+=("$name")
  kubectl apply -f - >/dev/null <<EOF || nd_die "could not create pod $name"
apiVersion: v1
kind: Pod
metadata:
  name: $name
  namespace: $ND_DEMO_NS
  labels: {$ND_LABEL_KEY: "true", app: $name}
spec:
  nodeName: $node
  $spec
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  tolerations: [{operator: Exists}]
  containers:
  - name: t
    image: $ND_TOOL_IMAGE
    command: ["sleep", "3600"]
    securityContext: {$sc}
EOF
  kubectl -n "$ND_DEMO_NS" wait --for=condition=Ready "pod/$name" --timeout=180s >/dev/null \
    || nd_die "pod $name not Ready"
}

# nd_x <pod> <shell command>: run in a repro pod and return its output.
nd_x() { kubectl -n "$ND_DEMO_NS" exec "$1" -- sh -c "$2"; }

# nd_bg <pod> <seconds> <shell command>: run a loop in a pod in the background.
# The pod-side loop stops by itself after <seconds>; the local exec is killed on exit.
nd_bg() {
  local pod=$1 secs=$2 cmd=$3 log
  log="$ND_LOGDIR/${pod}-$(date +%s%N).log"
  timeout "$((secs + 30))" kubectl -n "$ND_DEMO_NS" exec "$pod" -- \
    sh -c ": nd-repro-loop; end=\$(( \$(date +%s) + $secs )); $cmd" >"$log" 2>&1 &
  ND_BG_PIDS+=("$!")
}

# ---------------------------------------------------------------- backend
nd_backend_connect() {
  kubectl -n "$ND_NS" port-forward "svc/$ND_BACKEND_SVC" "$ND_PF_PORT:8080" \
    >"$ND_LOGDIR/port-forward.log" 2>&1 &
  ND_BG_PIDS+=("$!")
  local i
  for i in $(seq 1 30); do
    curl -fsS "http://127.0.0.1:$ND_PF_PORT/healthz" >/dev/null 2>&1 && return 0
    sleep 1
  done
  nd_die "backend port-forward did not come up (see $ND_LOGDIR/port-forward.log)"
}

# nd_require_quiet "<rule ids>": stop if one of these rules still has a firing
# incident. A new fault would merge into that alert and create no new
# incident, so the run could not be judged. Wait for it to resolve first.
nd_require_quiet() {
  local busy
  busy=$(curl -fsS "http://127.0.0.1:$ND_PF_PORT/incidents" | python3 -c "
import json, sys
rules = set(sys.argv[1].split())
for i in json.load(sys.stdin)['incidents']:
    if i['rule_id'] in rules and i.get('alert_status') == 'firing':
        print(i['rule_id'], i['incident_id'], 'since', i['starts_at'])" "$1")
  if [[ -n "$busy" ]]; then
    nd_log "still firing from an earlier run:"
    printf '  %s\n' "$busy" >&2
    nd_die "wait until it resolves (usually 5-15 min after the fault ends), then rerun"
  fi
}

# nd_status <since> "<rule ids>" [--final]
# Prints one line per expected rule. Exit 0 when every rule has an incident
# whose investigation finished (directly or through its correlation group).
nd_status() {
  python3 - "http://127.0.0.1:$ND_PF_PORT" "$@" <<'PY'
import json, sys, urllib.request

base, since, rules = sys.argv[1], sys.argv[2], sys.argv[3].split()
final = "--final" in sys.argv[4:]

def get(path):
    with urllib.request.urlopen(base + path, timeout=10) as r:
        return json.load(r)

items = [i for i in get("/incidents")["incidents"] if i["starts_at"] >= since]
full = {i["incident_id"]: get("/incidents/" + i["incident_id"]) for i in items}

ok = True
for rule in rules:
    cands = sorted((i for i in full.values() if i["rule_id"] == rule),
                   key=lambda i: i["starts_at"], reverse=True)
    if not cands:
        ok = False
        print(f"  {rule:7s} WAIT   no incident yet")
        continue
    inc = cands[0]
    owner = inc
    if inc.get("holmes_status") == "grouped" and inc.get("correlation_id"):
        owner = full.get(inc["correlation_id"]) or get("/incidents/" + inc["correlation_id"])
    st = owner.get("holmes_status") or "-"
    res = owner.get("holmes_result") or {}
    picked = res.get("scenario", "-")
    via = f" via {owner['incident_id']}" if owner is not inc else ""
    if st != "done":
        ok = False
        verdict = "FAIL" if st in ("failed", "skipped") else "WAIT"
    elif via and owner["rule_id"] not in rules:
        verdict = "WARN"   # grouped under an incident this repro did not expect
        via += f" ({owner['rule_id']}, unexpected)"
    elif picked == inc["scenario"] or (via and picked == owner["scenario"]):
        verdict = "PASS"
    else:
        verdict = "WARN"   # finished, but the answer names another scenario
    variant = inc.get("alert_labels", {}).get("variant")
    tag = f" [{variant}]" if variant else ""
    print(f"  {rule:7s} {verdict:6s} {inc['incident_id']}{tag}  holmes={st}{via}  "
          f"skill={picked} (expected {inc['scenario']})  "
          f"status={res.get('investigation_status', '-')} conf={res.get('confidence', '-')} "
          f"calls={owner.get('holmes_tool_calls', 0)}")
    if final:
        rc = (res.get("root_cause") or owner.get("holmes_error") or "").replace("\n", " ")
        print(f"          root_cause: {rc[:280]}")
sys.exit(0 if ok else 1)
PY
}

# nd_wait <since> "<rule ids>" <max minutes>
# Polls until every rule passes nd_status or time runs out, then prints the
# final table. Returns non-zero if any rule did not finish.
nd_wait() {
  local since=$1 rules=$2 max=$3 end
  end=$(($(date +%s) + max * 60))
  nd_log "waiting up to ${max}m for: $rules"
  while (($(date +%s) < end)); do
    if ((ND_FAULT_END && $(date +%s) >= ND_FAULT_END)); then nd_restore; ND_FAULT_END=0; fi
    if nd_status "$since" "$rules" >"$ND_LOGDIR/status.txt" 2>&1; then break; fi
    nd_log "$(grep -cE ' (WAIT|FAIL) ' "$ND_LOGDIR/status.txt") of $(wc -w <<<"$rules") rules pending"
    sleep 20
  done
  echo
  echo "== NetworkDoctor repro result (since $since)"
  nd_status "$since" "$rules" --final
}
