#!/usr/bin/env bash
# Rule 3 · conntrack-exhaustion
# Rule 5 · dns-conntrack-correlation
#
# A privileged hostNetwork pod on ND_NODE lowers nf_conntrack_max every 10s so
# usage stays near 85% of the limit (high, but never full, so the node keeps
# working). In parallel it sends uncached lookups to 1.1.1.1 from the host,
# which the eBPF agent counts as slow DNS (>50ms) on the same node.
#
# Both alerts fire on the same node, so the backend correlates them into one
# group and Holmes is called once. The original nf_conntrack_max is restored
# on exit. Rule 3 is often judged "excluded": a good investigation notices
# that the limit itself was lowered.
set -uo pipefail
. "$(dirname "$0")/lib.sh"

DURATION=${ND_DURATION:-480}
WAIT_MIN=${ND_WAIT_MIN:-18}
FILL_PCT=${ND_CONNTRACK_PCT:-85}

nd_preflight
nd_backend_connect
nd_require_quiet "rule-3 rule-5"
NODE=$(nd_node)

nd_pod nd-r35-node "$NODE" host
ORIG_MAX=$(nd_x nd-r35-node 'cat /proc/sys/net/netfilter/nf_conntrack_max')
[[ "$ORIG_MAX" =~ ^[0-9]+$ ]] || nd_die "could not read nf_conntrack_max"
nd_on_exit "nd_x nd-r35-node 'sysctl -qw net.netfilter.nf_conntrack_max=$ORIG_MAX'"
nd_log "node=$NODE conntrack_max=$ORIG_MAX target=${FILL_PCT}% duration=${DURATION}s"

nd_truth rule-3 node "$NODE"
SINCE=$(nd_now)
nd_fault_window "$DURATION"
# Rule 3 + Rule 5 conntrack side. The loop restores the limit itself when it
# ends; the undo step covers Ctrl+C.
nd_bg nd-r35-node "$DURATION" "
  while [ \$(date +%s) -lt \$end ]; do
    c=\$(cat /proc/sys/net/netfilter/nf_conntrack_count)
    sysctl -qw net.netfilter.nf_conntrack_max=\$(( c * 100 / $FILL_PCT + 20 ))
    sleep 10
  done
  sysctl -qw net.netfilter.nf_conntrack_max=$ORIG_MAX"
# Rule 5 DNS side: host-level lookups, slow because every name is a cache miss.
nd_bg nd-r35-node "$DURATION" "
  while [ \$(date +%s) -lt \$end ]; do
    dig +tries=1 +time=3 @1.1.1.1 \"h\$RANDOM\$RANDOM.wikipedia.org\" >/dev/null; sleep 0.3
  done"

sleep 30
nd_log "conntrack now $(nd_x nd-r35-node 'echo $(cat /proc/sys/net/netfilter/nf_conntrack_count)/$(cat /proc/sys/net/netfilter/nf_conntrack_max)')"
nd_wait "$SINCE" "rule-3 rule-5" "$WAIT_MIN"
