#!/usr/bin/env bash
# Throwaway HolmesGPT instances for comparison runs (eval/ablation.py).
#
#   holmes-variant.sh up raw              no NetworkDoctor skills (plain HolmesGPT)
#   holmes-variant.sh up ref <git-ref>    skills fetched from <git-ref> instead of main
#   holmes-variant.sh down raw|ref        remove the instance
#
# Each instance is a second render of deploy/helm/holmesgpt with the same model,
# LLM key, toolsets and MAX_STEPS as the production release; only the skills
# differ. It goes into its own namespace because the chart's
# custom-toolsets-configmap has a fixed name and would collide with the
# production release in the holmesgpt namespace. The LLM key Secret is copied
# from that namespace as-is.
#
# Reach it with: kubectl -n holmesgpt-<name> port-forward svc/holmesgpt-<name>-holmes <port>:80
#
# Needs helm and kubectl, and a repo checkout (ND_HOLMES_CHART points at the chart).
set -uo pipefail
. "$(dirname "$0")/lib.sh"

CHART=${ND_HOLMES_CHART:-$(cd "$(dirname "$0")/../../deploy/helm/holmesgpt" 2>/dev/null && pwd)}
SRC_NS=${ND_HOLMES_NS:-holmesgpt}
TIMEOUT=${ND_HOLMES_TIMEOUT:-180s}

usage() { sed -n '2,6p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }

check_env() {
  command -v kubectl >/dev/null || nd_die "kubectl not found"
  command -v helm >/dev/null || nd_die "helm not found"
  [[ -f "$CHART/Chart.yaml" ]] || nd_die "chart not found at '$CHART' (set ND_HOLMES_CHART)"
  local ctx
  ctx=$(kubectl config current-context 2>/dev/null) || nd_die "no kubectl context"
  [[ "$ctx" =~ $ND_CONTEXT_DENY ]] && nd_die "refusing to run against context '$ctx'"
  nd_log "context=$ctx chart=$CHART"
}

# override <kind> [ref]: values that differ from the production release.
override() {
  if [[ "$1" == raw ]]; then
    cat <<'EOF'
holmes:
  toolsets:
    skills:
      enabled: false        # no skill catalog in the prompt
  customSkillPaths: []        # no skill files
  initContainers: []          # no git fetch
  additionalVolumes: []
  additionalVolumeMounts: []
EOF
  else
    cat <<EOF
holmes:
  initContainers:
    - name: fetch-skills
      image: alpine/git:2.49.1
      env:
        - {name: SKILLS_REPO, value: https://github.com/weeeeestern/NetworkDoctor.git}
        - {name: SKILLS_REF, value: "$2"}
      command:
        - sh
        - -c
        - |
          set -eu
          git clone --quiet --depth 1 --filter=blob:none --sparse --branch "\$SKILLS_REF" "\$SKILLS_REPO" /tmp/repo
          cd /tmp/repo
          git sparse-checkout set skills
          cp -r skills/. /skills/
          echo "fetched \$(ls /skills | wc -l) skills from \$SKILLS_REF @ \$(git rev-parse --short HEAD)"
      volumeMounts:
        - {name: networkdoctor-skills, mountPath: /skills}
EOF
  fi
}

up() {
  local kind=$1 ref=${2:-} name rel ns pod
  [[ "$kind" == raw || ( "$kind" == ref && -n "$ref" ) ]] || usage
  if [[ "$kind" == raw ]]; then name=raw; else name=test; fi
  rel=holmesgpt-$name ns=holmesgpt-$name
  check_env
  kubectl get ns "$ns" >/dev/null 2>&1 && nd_die "$ns already exists; run '$0 down $kind' first"

  override "$kind" "$ref" >"$ND_LOGDIR/override.yaml"
  helm template "$rel" "$CHART" --namespace "$ns" \
    -f "$CHART/values.yaml" -f "$CHART/values-onprem-lab.yaml" -f "$ND_LOGDIR/override.yaml" \
    >"$ND_LOGDIR/render.yaml" || nd_die "helm template failed"

  kubectl create namespace "$ns" >/dev/null || nd_die "could not create $ns"
  kubectl -n "$SRC_NS" get secret holmes-llm -o json \
    | python3 -c 'import json,sys; s=json.load(sys.stdin); s["metadata"]={"name":s["metadata"]["name"]}; print(json.dumps(s))' \
    | kubectl -n "$ns" apply -f - >/dev/null || nd_die "could not copy Secret holmes-llm from $SRC_NS"
  kubectl -n "$ns" apply -f "$ND_LOGDIR/render.yaml" >/dev/null || nd_die "apply failed (render: $ND_LOGDIR/render.yaml)"
  kubectl -n "$ns" rollout status "deploy/$rel-holmes" --timeout="$TIMEOUT" >/dev/null \
    || nd_die "$rel did not become ready"

  pod=$(kubectl -n "$ns" get pod -o name | head -1)
  if [[ "$kind" == raw ]]; then
    kubectl -n "$ns" exec "$pod" -- test -e /etc/holmes/networkdoctor-skills 2>/dev/null \
      && nd_die "skills directory exists in the raw instance"
    nd_log "skills directory absent (as intended)"
  else
    nd_log "$(kubectl -n "$ns" logs "$pod" -c fetch-skills | tail -1)"
  fi
  nd_log "ready: kubectl -n $ns port-forward svc/$rel-holmes <port>:80"
}

down() {
  local kind=${1:-} name rel ns
  [[ "$kind" == raw || "$kind" == ref ]] || usage
  if [[ "$kind" == raw ]]; then name=raw; else name=test; fi
  rel=holmesgpt-$name ns=holmesgpt-$name
  command -v kubectl >/dev/null || nd_die "kubectl not found"
  kubectl delete ns "$ns" --ignore-not-found >/dev/null
  # Cluster-scoped objects are not removed with the namespace.
  kubectl delete clusterrolebinding "$rel-holmes-cluster-role-binding" --ignore-not-found >/dev/null
  kubectl delete clusterrole "$rel-holmes-cluster-role" --ignore-not-found >/dev/null
  nd_log "removed $ns and its cluster role"
}

case "${1:-}" in
  up)   shift; up "$@" ;;
  down) shift; down "$@" ;;
  *)    usage ;;
esac
