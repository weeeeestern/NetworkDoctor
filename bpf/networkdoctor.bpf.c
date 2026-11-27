// SPDX-License-Identifier: GPL-2.0
/*
 * networkdoctor.bpf.c
 *
 * NetworkDoctor eBPF kernel-side collector.
 *
 * Direct eBPF metrics covered:
 *   - ebpf_tcp_srtt_microseconds
 *   - ebpf_tcp_cwnd
 *   - eBPF runqlat
 *   - ebpf_dns_query_latency
 *   - ebpf_udp_session_duration
 *   - ebpf_tcp_connections_active
 *   - ebpf_tcp_connections_closed
 *
 * Auxiliary evidence covered:
 *   - flow-level TCP retransmission evidence
 *   - TCP connect failure evidence
 *   - TCP TIME_WAIT state-count evidence
 *   - DNS rcode evidence for SERVFAIL/NXDOMAIN correlation
 *
 * Build assumption:
 *   bpftool btf dump file /sys/kernel/btf/vmlinux format c > vmlinux.h
 *   clang -O2 -g -target bpf -D__TARGET_ARCH_x86 \
 *     -c networkdoctor.bpf.c -o networkdoctor.bpf.o
 *
 * Loader responsibility:
 *   - attach raw tracepoints: inet_sock_set_state, tcp_retransmit_skb,
 *     sched_wakeup, sched_wakeup_new, sched_switch
 *   - attach kprobe: tcp_rcv_established
 *   - attach nd_tc_ingress to TC ingress and nd_tc_egress to TC egress
 *     (tcx head on Linux 6.6+, clsact filters otherwise)
 *   - read per-CPU maps by summing CPU values
 *   - scan nd_dns_pending and nd_udp_flows for timeout/idle expiration
 */

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_endian.h>

char LICENSE[] SEC("license") = "GPL";

#ifndef AF_INET
#define AF_INET 2
#endif

#ifndef ETH_P_IP
#define ETH_P_IP 0x0800
#endif

#ifndef IPPROTO_TCP
#define IPPROTO_TCP 6
#endif

#ifndef IPPROTO_UDP
#define IPPROTO_UDP 17
#endif

/* TC_ACT_UNSPEC passes the packet on to the next program: the next tcx
 * program (for example Cilium's) or, at the end of the chain, the default
 * action. TC_ACT_OK would end the chain and skip the programs behind us. */
#ifndef TC_ACT_UNSPEC
#define TC_ACT_UNSPEC (-1)
#endif

/* Linux enum tcp_state values. */
#ifndef TCP_ESTABLISHED
#define TCP_ESTABLISHED 1
#endif
#ifndef TCP_SYN_SENT
#define TCP_SYN_SENT 2
#endif
#ifndef TCP_SYN_RECV
#define TCP_SYN_RECV 3
#endif
#ifndef TCP_FIN_WAIT1
#define TCP_FIN_WAIT1 4
#endif
#ifndef TCP_FIN_WAIT2
#define TCP_FIN_WAIT2 5
#endif
#ifndef TCP_TIME_WAIT
#define TCP_TIME_WAIT 6
#endif
#ifndef TCP_CLOSE
#define TCP_CLOSE 7
#endif
#ifndef TCP_CLOSE_WAIT
#define TCP_CLOSE_WAIT 8
#endif
#ifndef TCP_LAST_ACK
#define TCP_LAST_ACK 9
#endif
#ifndef TCP_LISTEN
#define TCP_LISTEN 10
#endif
#ifndef TCP_CLOSING
#define TCP_CLOSING 11
#endif
#ifndef TCP_NEW_SYN_RECV
#define TCP_NEW_SYN_RECV 12
#endif

#define ND_HIST_BUCKETS 32
#define ND_TCP_STATES   16
#define ND_DNS_RCODES   16

#define ND_DIR_UNKNOWN 0
#define ND_DIR_INGRESS 1
#define ND_DIR_EGRESS  2

#define ND_DEFAULT_SLOW_TCP_RTT_US       100000ULL   /* 100ms */
#define ND_DEFAULT_SHORT_TCP_CONN_US     100000ULL   /* 100ms */
#define ND_DEFAULT_SLOW_DNS_US            50000ULL   /* 50ms */
#define ND_DEFAULT_LONG_UDP_FLOW_US    30000000ULL   /* 30s */
#define ND_DEFAULT_HIGH_RUNQLAT_US         5000ULL   /* 5ms */
#define ND_DEFAULT_RETRANS_WINDOW_NS 1000000000ULL   /* 1s */
#define ND_DEFAULT_EVENT_COOLDOWN_NS 5000000000ULL   /* 5s */
#define ND_DEFAULT_RETRANS_BURST_COUNT        3ULL
#define ND_DEFAULT_CWND_DROP_PERCENT         50ULL   /* current <= prev * 50% */

#define ND_DNS_QR_RESPONSE 0x8000
#define ND_DNS_RCODE_MASK  0x000f
#define ND_DNS_HDR_LEN     12

/* IPv4 fragmentation bits after ntohs(ip->frag_off). */
#define ND_IP_MF_OR_OFFSET 0x3fff

enum nd_event_type {
    ND_EVT_SLOW_TCP_RTT       = 1,
    ND_EVT_TCP_CWND_DROP      = 2,
    ND_EVT_TCP_RETRANS_BURST  = 3,
    ND_EVT_SHORT_TCP_CONN     = 4,
    ND_EVT_TCP_CONNECT_FAIL   = 5,
    ND_EVT_SLOW_DNS           = 6,
    ND_EVT_DNS_RCODE_ERROR    = 7,
    ND_EVT_LONG_UDP_FLOW      = 8,
    ND_EVT_HIGH_RUNQLAT       = 9,
};

struct nd_config {
    __u64 slow_tcp_rtt_us;
    __u64 short_tcp_conn_us;
    __u64 slow_dns_us;
    __u64 long_udp_flow_us;
    __u64 high_runqlat_us;
    __u64 retrans_burst_window_ns;
    __u64 event_cooldown_ns;
    __u64 retrans_burst_count;
    __u64 cwnd_drop_percent;
};

struct nd_flow4_key {
    __u32 ifindex;
    __u32 src_ip;      /* network byte order */
    __u32 dst_ip;      /* network byte order */
    __u16 src_port;    /* host byte order */
    __u16 dst_port;    /* host byte order */
    __u8  proto;
    __u8  direction;   /* ND_DIR_* */
    __u16 pad;
};

struct nd_tcp_counters {
    __u64 closed_total;
    __u64 short_lived_total;
    __u64 connect_attempt_total;
    __u64 connect_failed_total;
    __u64 retrans_total;
    __u64 retrans_burst_total;
    __u64 rtt_sample_total;
    __u64 cwnd_sample_total;
};

struct nd_dns_counters {
    __u64 query_total;
    __u64 response_total;
    __u64 rcode_error_total;
    __u64 slow_total;
};

struct nd_udp_counters {
    __u64 packet_total;
    __u64 long_flow_total;
};

struct nd_tcp_conn_life {
    __u64 start_ns;
    __u64 last_ns;
    __u32 src_ip;
    __u32 dst_ip;
    __u16 src_port;
    __u16 dst_port;
    __u8  initial_state;
    __u8  last_state;
    __u16 pad;
};

struct nd_tcp_flow_value {
    __u64 last_seen_ns;
    __u64 last_rtt_event_ns;
    __u64 last_cwnd_event_ns;
    __u32 srtt_us;
    __u32 cwnd;
    __u32 prev_cwnd;
    __u32 retrans_count;
};

struct nd_retrans_value {
    __u64 first_seen_ns;
    __u64 last_seen_ns;
    __u64 count;
    __u64 last_event_ns;
};

struct nd_dns_key {
    __u32 ifindex;
    __u32 client_ip;    /* network byte order */
    __u32 server_ip;    /* network byte order */
    __u16 client_port;  /* host byte order */
    __u16 server_port;  /* host byte order, usually 53 */
    __u16 txid;         /* host byte order */
    __u16 pad;
};

struct nd_dns_pending_value {
    __u64 start_ns;
};

struct nd_udp_flow_value {
    __u64 first_seen_ns;
    __u64 last_seen_ns;
    __u64 last_event_ns;
    __u64 packets;
    __u64 bytes;
};

struct nd_event {
    __u32 type;
    __u32 pid;
    __u64 ts_ns;

    __u32 ifindex;
    __u32 src_ip;
    __u32 dst_ip;
    __u16 src_port;
    __u16 dst_port;

    /*
     * event-specific meaning:
     *   SLOW_TCP_RTT:      value_us = srtt_us, aux = cwnd
     *   TCP_CWND_DROP:     value_us = current_cwnd, aux = prev_cwnd
     *   RETRANS_BURST:     value_us = retrans_count, aux = window_ns
     *   SHORT_TCP_CONN:    value_us = duration_us
     *   TCP_CONNECT_FAIL:  value_us = last_state
     *   SLOW_DNS:          value_us = dns_latency_us, aux = rcode
     *   DNS_RCODE_ERROR:   value_us = rcode
     *   LONG_UDP_FLOW:     value_us = duration_us, aux = packets
     *   HIGH_RUNQLAT:      value_us = runqlat_us, aux = pid
     */
    __u64 value_us;
    __u64 aux;
};

struct nd_dns_hdr {
    __be16 id;
    __be16 flags;
    __be16 qdcount;
    __be16 ancount;
    __be16 nscount;
    __be16 arcount;
};

/* ----------------------------- BPF maps ----------------------------- */

struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, struct nd_config);
} nd_config SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, struct nd_tcp_counters);
} nd_tcp_counters SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, struct nd_dns_counters);
} nd_dns_counters SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, struct nd_udp_counters);
} nd_udp_counters SEC(".maps");

/* key = Linux TCP state number; value = signed state-count delta. */
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, ND_TCP_STATES);
    __type(key, __u32);
    __type(value, __s64);
} nd_tcp_state_delta SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 131072);
    __type(key, __u64); /* socket pointer */
    __type(value, struct nd_tcp_conn_life);
} nd_tcp_conn_life SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 131072);
    __type(key, struct nd_flow4_key);
    __type(value, struct nd_tcp_flow_value);
} nd_tcp_flow_latest SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, ND_HIST_BUCKETS);
    __type(key, __u32);
    __type(value, __u64);
} nd_tcp_srtt_us_hist SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, ND_HIST_BUCKETS);
    __type(key, __u32);
    __type(value, __u64);
} nd_tcp_cwnd_hist SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 131072);
    __type(key, struct nd_flow4_key);
    __type(value, struct nd_retrans_value);
} nd_tcp_retrans_by_flow SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 131072);
    __type(key, struct nd_dns_key);
    __type(value, struct nd_dns_pending_value);
} nd_dns_pending SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, ND_HIST_BUCKETS);
    __type(key, __u32);
    __type(value, __u64);
} nd_dns_latency_us_hist SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, ND_DNS_RCODES);
    __type(key, __u32);
    __type(value, __u64);
} nd_dns_rcode_total SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 131072);
    __type(key, struct nd_flow4_key);
    __type(value, struct nd_udp_flow_value);
} nd_udp_flows SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, ND_HIST_BUCKETS);
    __type(key, __u32);
    __type(value, __u64);
} nd_udp_duration_us_hist SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 131072);
    __type(key, __u32);   /* pid */
    __type(value, __u64); /* wakeup timestamp ns */
} nd_runq_start SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, ND_HIST_BUCKETS);
    __type(key, __u32);
    __type(value, __u64);
} nd_runqlat_us_hist SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 1 << 20);
    /*
     * Ring buffer maps don't use key/value layout at runtime, but bpf2go needs
     * this BTF hint to generate the userspace Go type for struct nd_event.
     */
    __type(value, struct nd_event);
} nd_events SEC(".maps");

/* ----------------------------- helpers ------------------------------ */

static __always_inline struct nd_config nd_get_config(void)
{
    struct nd_config out = {
        .slow_tcp_rtt_us = ND_DEFAULT_SLOW_TCP_RTT_US,
        .short_tcp_conn_us = ND_DEFAULT_SHORT_TCP_CONN_US,
        .slow_dns_us = ND_DEFAULT_SLOW_DNS_US,
        .long_udp_flow_us = ND_DEFAULT_LONG_UDP_FLOW_US,
        .high_runqlat_us = ND_DEFAULT_HIGH_RUNQLAT_US,
        .retrans_burst_window_ns = ND_DEFAULT_RETRANS_WINDOW_NS,
        .event_cooldown_ns = ND_DEFAULT_EVENT_COOLDOWN_NS,
        .retrans_burst_count = ND_DEFAULT_RETRANS_BURST_COUNT,
        .cwnd_drop_percent = ND_DEFAULT_CWND_DROP_PERCENT,
    };

    __u32 k = 0;
    struct nd_config *cfg = bpf_map_lookup_elem(&nd_config, &k);
    if (!cfg)
        return out;

    if (cfg->slow_tcp_rtt_us)
        out.slow_tcp_rtt_us = cfg->slow_tcp_rtt_us;
    if (cfg->short_tcp_conn_us)
        out.short_tcp_conn_us = cfg->short_tcp_conn_us;
    if (cfg->slow_dns_us)
        out.slow_dns_us = cfg->slow_dns_us;
    if (cfg->long_udp_flow_us)
        out.long_udp_flow_us = cfg->long_udp_flow_us;
    if (cfg->high_runqlat_us)
        out.high_runqlat_us = cfg->high_runqlat_us;
    if (cfg->retrans_burst_window_ns)
        out.retrans_burst_window_ns = cfg->retrans_burst_window_ns;
    if (cfg->event_cooldown_ns)
        out.event_cooldown_ns = cfg->event_cooldown_ns;
    if (cfg->retrans_burst_count)
        out.retrans_burst_count = cfg->retrans_burst_count;
    if (cfg->cwnd_drop_percent)
        out.cwnd_drop_percent = cfg->cwnd_drop_percent;

    return out;
}

static __always_inline __u32 nd_log2_bucket(__u64 v)
{
    if (v == 0)
        return 0;

#pragma unroll
    for (int i = 0; i < ND_HIST_BUCKETS - 1; i++) {
        if (v <= (1ULL << i))
            return i;
    }
    return ND_HIST_BUCKETS - 1;
}

static __always_inline void nd_inc_u64_percpu_array(void *map, __u32 key)
{
    __u64 *v = bpf_map_lookup_elem(map, &key);
    if (v)
        *v += 1;
}

static __always_inline void nd_add_s64_percpu_array(void *map, __u32 key, __s64 delta)
{
    __s64 *v = bpf_map_lookup_elem(map, &key);
    if (v)
        *v += delta;
}

static __always_inline void nd_inc_tcp_counter(__u32 field_id)
{
    __u32 k = 0;
    struct nd_tcp_counters *c = bpf_map_lookup_elem(&nd_tcp_counters, &k);
    if (!c)
        return;

    if (field_id == 0)
        c->closed_total++;
    else if (field_id == 1)
        c->short_lived_total++;
    else if (field_id == 2)
        c->connect_attempt_total++;
    else if (field_id == 3)
        c->connect_failed_total++;
    else if (field_id == 4)
        c->retrans_total++;
    else if (field_id == 5)
        c->retrans_burst_total++;
    else if (field_id == 6)
        c->rtt_sample_total++;
    else if (field_id == 7)
        c->cwnd_sample_total++;
}

static __always_inline void nd_inc_dns_counter(__u32 field_id)
{
    __u32 k = 0;
    struct nd_dns_counters *c = bpf_map_lookup_elem(&nd_dns_counters, &k);
    if (!c)
        return;

    if (field_id == 0)
        c->query_total++;
    else if (field_id == 1)
        c->response_total++;
    else if (field_id == 2)
        c->rcode_error_total++;
    else if (field_id == 3)
        c->slow_total++;
}

static __always_inline void nd_inc_udp_counter(__u32 field_id)
{
    __u32 k = 0;
    struct nd_udp_counters *c = bpf_map_lookup_elem(&nd_udp_counters, &k);
    if (!c)
        return;

    if (field_id == 0)
        c->packet_total++;
    else if (field_id == 1)
        c->long_flow_total++;
}

static __always_inline void nd_fill_event(struct nd_event *e,
                                          __u32 type,
                                          const struct nd_flow4_key *flow,
                                          __u64 value_us,
                                          __u64 aux,
                                          __u32 pid)
{
    e->type = type;
    e->pid = pid;
    e->ts_ns = bpf_ktime_get_ns();
    e->ifindex = 0;
    e->src_ip = 0;
    e->dst_ip = 0;
    e->src_port = 0;
    e->dst_port = 0;
    e->value_us = value_us;
    e->aux = aux;

    if (flow) {
        e->ifindex = flow->ifindex;
        e->src_ip = flow->src_ip;
        e->dst_ip = flow->dst_ip;
        e->src_port = flow->src_port;
        e->dst_port = flow->dst_port;
    }
}

static __always_inline void nd_submit_event(__u32 type,
                                            const struct nd_flow4_key *flow,
                                            __u64 value_us,
                                            __u64 aux)
{
    struct nd_event *e = bpf_ringbuf_reserve(&nd_events, sizeof(*e), 0);
    if (!e)
        return;

    nd_fill_event(e, type, flow, value_us, aux,
                  (__u32)(bpf_get_current_pid_tgid() >> 32));
    bpf_ringbuf_submit(e, 0);
}

static __always_inline void nd_submit_event_no_pid(__u32 type,
                                                   const struct nd_flow4_key *flow,
                                                   __u64 value_us,
                                                   __u64 aux)
{
    struct nd_event *e = bpf_ringbuf_reserve(&nd_events, sizeof(*e), 0);
    if (!e)
        return;

    nd_fill_event(e, type, flow, value_us, aux, 0);
    bpf_ringbuf_submit(e, 0);
}

static __always_inline int nd_is_ipv4_sock(const struct sock *sk)
{
    __u16 family = BPF_CORE_READ(sk, __sk_common.skc_family);
    return family == AF_INET;
}

static __always_inline int nd_flow4_from_sock(const struct sock *sk,
                                              __u8 proto,
                                              __u8 direction,
                                              struct nd_flow4_key *key)
{
    if (!sk || !key)
        return 0;

    if (!nd_is_ipv4_sock(sk))
        return 0;

    key->ifindex = 0;
    key->src_ip = BPF_CORE_READ(sk, __sk_common.skc_rcv_saddr);
    key->dst_ip = BPF_CORE_READ(sk, __sk_common.skc_daddr);
    key->src_port = BPF_CORE_READ(sk, __sk_common.skc_num);

    __be16 dport = BPF_CORE_READ(sk, __sk_common.skc_dport);
    key->dst_port = bpf_ntohs(dport);
    key->proto = proto;
    key->direction = direction;
    key->pad = 0;

    return 1;
}

static __always_inline void nd_tcp_state_delta_add(int state, __s64 delta)
{
    if (state < 0 || state >= ND_TCP_STATES)
        return;

    __u32 key = (__u32)state;
    nd_add_s64_percpu_array(&nd_tcp_state_delta, key, delta);
}

static __always_inline int nd_is_syn_state(int state)
{
    return state == TCP_SYN_SENT || state == TCP_SYN_RECV || state == TCP_NEW_SYN_RECV;
}

/* ----------------------- TCP state lifecycle hook -------------------- */

SEC("raw_tp/inet_sock_set_state")
int nd_inet_sock_set_state(struct bpf_raw_tracepoint_args *ctx)
{
    const struct sock *sk = (const struct sock *)ctx->args[0];
    int oldstate = (int)ctx->args[1];
    int newstate = (int)ctx->args[2];
    __u64 now = bpf_ktime_get_ns();
    struct nd_config cfg = nd_get_config();

    if (!sk || !nd_is_ipv4_sock(sk))
        return 0;

    nd_tcp_state_delta_add(oldstate, -1);
    nd_tcp_state_delta_add(newstate, 1);

    if (newstate == TCP_SYN_SENT || newstate == TCP_SYN_RECV)
        nd_inc_tcp_counter(2); /* connect_attempt_total */

    struct nd_flow4_key flow = {};
    int have_flow = nd_flow4_from_sock(sk, IPPROTO_TCP, ND_DIR_UNKNOWN, &flow);
    __u64 sk_key = (__u64)(unsigned long)sk;

    if (newstate == TCP_ESTABLISHED) {
        struct nd_tcp_conn_life life = {
            .start_ns = now,
            .last_ns = now,
            .src_ip = flow.src_ip,
            .dst_ip = flow.dst_ip,
            .src_port = flow.src_port,
            .dst_port = flow.dst_port,
            .initial_state = (__u8)newstate,
            .last_state = (__u8)newstate,
            .pad = 0,
        };
        bpf_map_update_elem(&nd_tcp_conn_life, &sk_key, &life, BPF_ANY);
        return 0;
    }

    /* Count a connection as closed once it leaves ESTABLISHED. */
    if (oldstate == TCP_ESTABLISHED && newstate != TCP_ESTABLISHED) {
        struct nd_tcp_conn_life *life = bpf_map_lookup_elem(&nd_tcp_conn_life, &sk_key);
        if (life) {
            __u64 dur_us = 0;
            if (now > life->start_ns)
                dur_us = (now - life->start_ns) / 1000ULL;

            nd_inc_tcp_counter(0); /* closed_total */

            if (dur_us > 0 && dur_us < cfg.short_tcp_conn_us) {
                nd_inc_tcp_counter(1); /* short_lived_total */
                if (have_flow)
                    nd_submit_event(ND_EVT_SHORT_TCP_CONN, &flow, dur_us, 0);
            }

            bpf_map_delete_elem(&nd_tcp_conn_life, &sk_key);
        }
        return 0;
    }

    /* Failed connect: SYN-like state went directly to CLOSE without ESTABLISHED. */
    if (nd_is_syn_state(oldstate) && newstate == TCP_CLOSE) {
        nd_inc_tcp_counter(3); /* connect_failed_total */
        if (have_flow)
            nd_submit_event(ND_EVT_TCP_CONNECT_FAIL, &flow, (__u64)oldstate, (__u64)newstate);
    }

    return 0;
}

/* ----------------------- TCP RTT / CWND hook ------------------------- */

SEC("kprobe/tcp_rcv_established")
int BPF_KPROBE(nd_tcp_rcv_established, struct sock *sk, struct sk_buff *skb)
{
    (void)skb;

    if (!sk || !nd_is_ipv4_sock(sk))
        return 0;

    struct nd_config cfg = nd_get_config();
    __u64 now = bpf_ktime_get_ns();

    struct tcp_sock *tp = (struct tcp_sock *)sk;
    __u32 srtt_raw = BPF_CORE_READ(tp, srtt_us);
    __u32 srtt_us = srtt_raw >> 3; /* Linux stores srtt_us scaled by 8. */
    __u32 cwnd = BPF_CORE_READ(tp, snd_cwnd);

    if (srtt_us > 0) {
        __u32 b = nd_log2_bucket((__u64)srtt_us);
        nd_inc_u64_percpu_array(&nd_tcp_srtt_us_hist, b);
        nd_inc_tcp_counter(6); /* rtt_sample_total */
    }

    if (cwnd > 0) {
        __u32 b = nd_log2_bucket((__u64)cwnd);
        nd_inc_u64_percpu_array(&nd_tcp_cwnd_hist, b);
        nd_inc_tcp_counter(7); /* cwnd_sample_total */
    }

    struct nd_flow4_key flow = {};
    if (!nd_flow4_from_sock(sk, IPPROTO_TCP, ND_DIR_UNKNOWN, &flow))
        return 0;

    struct nd_tcp_flow_value newv = {
        .last_seen_ns = now,
        .last_rtt_event_ns = 0,
        .last_cwnd_event_ns = 0,
        .srtt_us = srtt_us,
        .cwnd = cwnd,
        .prev_cwnd = 0,
        .retrans_count = 0,
    };

    struct nd_tcp_flow_value *oldv = bpf_map_lookup_elem(&nd_tcp_flow_latest, &flow);
    if (oldv) {
        newv.prev_cwnd = oldv->cwnd;
        newv.retrans_count = oldv->retrans_count;
        newv.last_rtt_event_ns = oldv->last_rtt_event_ns;
        newv.last_cwnd_event_ns = oldv->last_cwnd_event_ns;
    }

    if (srtt_us >= cfg.slow_tcp_rtt_us &&
        now > newv.last_rtt_event_ns + cfg.event_cooldown_ns) {
        nd_submit_event(ND_EVT_SLOW_TCP_RTT, &flow, srtt_us, cwnd);
        newv.last_rtt_event_ns = now;
    }

    if (newv.prev_cwnd > 0 && cwnd > 0) {
        __u64 lhs = (__u64)cwnd * 100ULL;
        __u64 rhs = (__u64)newv.prev_cwnd * cfg.cwnd_drop_percent;
        if (lhs <= rhs && now > newv.last_cwnd_event_ns + cfg.event_cooldown_ns) {
            nd_submit_event(ND_EVT_TCP_CWND_DROP, &flow, cwnd, newv.prev_cwnd);
            newv.last_cwnd_event_ns = now;
        }
    }

    bpf_map_update_elem(&nd_tcp_flow_latest, &flow, &newv, BPF_ANY);
    return 0;
}

/* ----------------------- TCP retransmission hook --------------------- */

SEC("raw_tp/tcp_retransmit_skb")
int nd_tcp_retransmit_skb(struct bpf_raw_tracepoint_args *ctx)
{
    const struct sock *sk = (const struct sock *)ctx->args[0];
    if (!sk || !nd_is_ipv4_sock(sk))
        return 0;

    struct nd_config cfg = nd_get_config();
    __u64 now = bpf_ktime_get_ns();

    nd_inc_tcp_counter(4); /* retrans_total */

    struct nd_flow4_key flow = {};
    if (!nd_flow4_from_sock(sk, IPPROTO_TCP, ND_DIR_UNKNOWN, &flow))
        return 0;

    struct nd_retrans_value newv = {
        .first_seen_ns = now,
        .last_seen_ns = now,
        .count = 1,
        .last_event_ns = 0,
    };

    struct nd_retrans_value *oldv = bpf_map_lookup_elem(&nd_tcp_retrans_by_flow, &flow);
    if (oldv) {
        if (now > oldv->first_seen_ns &&
            now - oldv->first_seen_ns <= cfg.retrans_burst_window_ns) {
            newv.first_seen_ns = oldv->first_seen_ns;
            newv.count = oldv->count + 1;
        }
        newv.last_event_ns = oldv->last_event_ns;
    }

    if (newv.count >= cfg.retrans_burst_count &&
        now > newv.last_event_ns + cfg.event_cooldown_ns) {
        nd_inc_tcp_counter(5); /* retrans_burst_total */
        nd_submit_event(ND_EVT_TCP_RETRANS_BURST, &flow, newv.count,
                        cfg.retrans_burst_window_ns);
        newv.last_event_ns = now;
    }

    bpf_map_update_elem(&nd_tcp_retrans_by_flow, &flow, &newv, BPF_ANY);

    struct nd_tcp_flow_value *tv = bpf_map_lookup_elem(&nd_tcp_flow_latest, &flow);
    if (tv) {
        tv->retrans_count++;
        tv->last_seen_ns = now;
    }

    return 0;
}

/* ----------------------- TC UDP / DNS hook --------------------------- */

static __always_inline int nd_parse_ipv4_udp(struct __sk_buff *skb,
                                             __u8 direction)
{
    void *data = (void *)(long)skb->data;
    void *data_end = (void *)(long)skb->data_end;

    struct ethhdr *eth = data;
    if ((void *)(eth + 1) > data_end)
        return TC_ACT_UNSPEC;

    if (bpf_ntohs(eth->h_proto) != ETH_P_IP)
        return TC_ACT_UNSPEC;

    struct iphdr *ip = (void *)(eth + 1);
    if ((void *)(ip + 1) > data_end)
        return TC_ACT_UNSPEC;

    if (ip->protocol != IPPROTO_UDP)
        return TC_ACT_UNSPEC;

    __u16 frag = bpf_ntohs(ip->frag_off);
    if (frag & ND_IP_MF_OR_OFFSET)
        return TC_ACT_UNSPEC;

    __u32 ihl_bytes = (__u32)ip->ihl * 4;
    if (ihl_bytes < sizeof(*ip))
        return TC_ACT_UNSPEC;

    struct udphdr *udp = (void *)ip + ihl_bytes;
    if ((void *)(udp + 1) > data_end)
        return TC_ACT_UNSPEC;

    __u16 sport = bpf_ntohs(udp->source);
    __u16 dport = bpf_ntohs(udp->dest);
    __u64 now = bpf_ktime_get_ns();
    __u32 ifindex = skb->ifindex;

    struct nd_flow4_key flow = {
        .ifindex = ifindex,
        .src_ip = ip->saddr,
        .dst_ip = ip->daddr,
        .src_port = sport,
        .dst_port = dport,
        .proto = IPPROTO_UDP,
        .direction = direction,
        .pad = 0,
    };

    nd_inc_udp_counter(0); /* packet_total */

    struct nd_config cfg = nd_get_config();
    struct nd_udp_flow_value first = {
        .first_seen_ns = now,
        .last_seen_ns = now,
        .last_event_ns = 0,
        .packets = 1,
        .bytes = skb->len,
    };

    struct nd_udp_flow_value *uv = bpf_map_lookup_elem(&nd_udp_flows, &flow);
    if (!uv) {
        bpf_map_update_elem(&nd_udp_flows, &flow, &first, BPF_ANY);
    } else {
        uv->last_seen_ns = now;
        uv->packets++;
        uv->bytes += skb->len;

        __u64 dur_us = 0;
        if (now > uv->first_seen_ns)
            dur_us = (now - uv->first_seen_ns) / 1000ULL;

        if (dur_us >= cfg.long_udp_flow_us &&
            now > uv->last_event_ns + cfg.event_cooldown_ns) {
            __u32 b = nd_log2_bucket(dur_us);
            nd_inc_u64_percpu_array(&nd_udp_duration_us_hist, b);
            nd_inc_udp_counter(1); /* long_flow_total */
            nd_submit_event_no_pid(ND_EVT_LONG_UDP_FLOW, &flow, dur_us, uv->packets);
            uv->last_event_ns = now;
        }
    }

    if (sport != 53 && dport != 53)
        return TC_ACT_UNSPEC;

    struct nd_dns_hdr *dns = (void *)(udp + 1);
    if ((void *)dns + ND_DNS_HDR_LEN > data_end)
        return TC_ACT_UNSPEC;

    __u16 txid = bpf_ntohs(dns->id);
    __u16 flags = bpf_ntohs(dns->flags);
    __u16 rcode = flags & ND_DNS_RCODE_MASK;
    int is_response = (flags & ND_DNS_QR_RESPONSE) != 0;

    if (!is_response && dport == 53) {
        struct nd_dns_key key = {
            .ifindex = ifindex,
            .client_ip = ip->saddr,
            .server_ip = ip->daddr,
            .client_port = sport,
            .server_port = dport,
            .txid = txid,
            .pad = 0,
        };
        struct nd_dns_pending_value val = {
            .start_ns = now,
        };
        nd_inc_dns_counter(0); /* query_total */
        bpf_map_update_elem(&nd_dns_pending, &key, &val, BPF_ANY);
        return TC_ACT_UNSPEC;
    }

    if (is_response && sport == 53) {
        struct nd_dns_key key = {
            .ifindex = ifindex,
            .client_ip = ip->daddr,
            .server_ip = ip->saddr,
            .client_port = dport,
            .server_port = sport,
            .txid = txid,
            .pad = 0,
        };

        nd_inc_dns_counter(1); /* response_total */

        if (rcode < ND_DNS_RCODES) {
            __u32 rk = rcode;
            nd_inc_u64_percpu_array(&nd_dns_rcode_total, rk);
            if (rcode != 0) {
                nd_inc_dns_counter(2); /* rcode_error_total */
                nd_submit_event_no_pid(ND_EVT_DNS_RCODE_ERROR, &flow, rcode, 0);
            }
        }

        struct nd_dns_pending_value *pending = bpf_map_lookup_elem(&nd_dns_pending, &key);
        if (pending) {
            __u64 latency_us = 0;
            if (now > pending->start_ns)
                latency_us = (now - pending->start_ns) / 1000ULL;

            if (latency_us > 0) {
                __u32 b = nd_log2_bucket(latency_us);
                nd_inc_u64_percpu_array(&nd_dns_latency_us_hist, b);
            }

            if (latency_us >= cfg.slow_dns_us) {
                nd_inc_dns_counter(3); /* slow_total */
                nd_submit_event_no_pid(ND_EVT_SLOW_DNS, &flow, latency_us, rcode);
            }

            bpf_map_delete_elem(&nd_dns_pending, &key);
        }
    }

    return TC_ACT_UNSPEC;
}

SEC("tc")
int nd_tc_ingress(struct __sk_buff *skb)
{
    return nd_parse_ipv4_udp(skb, ND_DIR_INGRESS);
}

SEC("tc")
int nd_tc_egress(struct __sk_buff *skb)
{
    return nd_parse_ipv4_udp(skb, ND_DIR_EGRESS);
}

/* ----------------------- Scheduler runqlat hooks --------------------- */

static __always_inline int nd_sched_wakeup_common(struct task_struct *p)
{
    if (!p)
        return 0;

    __u32 pid = BPF_CORE_READ(p, pid);
    if (pid == 0)
        return 0;

    __u64 now = bpf_ktime_get_ns();
    bpf_map_update_elem(&nd_runq_start, &pid, &now, BPF_ANY);
    return 0;
}

SEC("raw_tp/sched_wakeup")
int nd_sched_wakeup(struct bpf_raw_tracepoint_args *ctx)
{
    struct task_struct *p = (struct task_struct *)ctx->args[0];
    return nd_sched_wakeup_common(p);
}

SEC("raw_tp/sched_wakeup_new")
int nd_sched_wakeup_new(struct bpf_raw_tracepoint_args *ctx)
{
    struct task_struct *p = (struct task_struct *)ctx->args[0];
    return nd_sched_wakeup_common(p);
}

SEC("raw_tp/sched_switch")
int nd_sched_switch(struct bpf_raw_tracepoint_args *ctx)
{
    /* TP_PROTO(bool preempt, struct task_struct *prev, struct task_struct *next, unsigned int prev_state) */
    struct task_struct *next = (struct task_struct *)ctx->args[2];
    if (!next)
        return 0;

    __u32 next_pid = BPF_CORE_READ(next, pid);
    if (next_pid == 0)
        return 0;

    __u64 *start = bpf_map_lookup_elem(&nd_runq_start, &next_pid);
    if (!start)
        return 0;

    __u64 now = bpf_ktime_get_ns();
    if (now > *start) {
        __u64 runqlat_us = (now - *start) / 1000ULL;
        __u32 b = nd_log2_bucket(runqlat_us);
        nd_inc_u64_percpu_array(&nd_runqlat_us_hist, b);

        struct nd_config cfg = nd_get_config();
        if (runqlat_us >= cfg.high_runqlat_us)
            nd_submit_event(ND_EVT_HIGH_RUNQLAT, 0, runqlat_us, next_pid);
    }

    bpf_map_delete_elem(&nd_runq_start, &next_pid);
    return 0;
}
