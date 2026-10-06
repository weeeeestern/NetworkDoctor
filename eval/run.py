#!/usr/bin/env python3
"""NetworkDoctor RCA evaluation harness (standard library only).

Compares investigation architectures on incidents whose cause is known,
because scripts/repro injected it:

  baseline      evidence snapshot + Holmes (the original pipeline)
  derived       + deterministic facts computed by the backend
  derived+jev   + a Jev consistency check and one re-examination on failure

Modes
  replay  re-investigate stored incidents (eval/cases.json) against the same
          Prometheus history. Cheap and repeatable; Kubernetes objects from
          the original run are gone, equally for every architecture.
  live    run one repro script, hold the fault, and investigate the new
          incident with every architecture while the fault is still live.
  report  score result files and print a Markdown comparison.

Examples (on a host with kubectl access, e.g. the lab control plane)
  python3 eval/run.py replay --archs baseline,derived --repeat 2
  python3 eval/run.py live scripts/repro/rule4-coredns.sh --archs baseline,derived
  python3 eval/run.py report eval/results/*.jsonl

The backend is reached through `kubectl port-forward` unless ND_BACKEND_URL
is set. Every run is one Holmes investigation (about $0.04 with gpt-luna).
"""

import argparse
import datetime as dt
import json
import os
import re
import signal
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
ARCHS = ["baseline", "derived", "derived+jev"]
SCRIPT_RULES = {  # repro script -> (expected rule ids, ground-truth template per rule)
    "rule1-congestion.sh": ["rule-1"],
    "rule2-8-nic-drop-policy.sh": ["rule-2", "rule-8"],
    "rule3-5-conntrack-dns.sh": ["rule-3", "rule-5"],
    "rule4-coredns.sh": ["rule-4"],
    "rule6-node-connect-fail.sh": ["rule-6"],
    "rule7-app-5xx.sh": ["rule-7"],
}


def now():
    return dt.datetime.now(dt.timezone.utc).replace(tzinfo=None)


def log(msg):
    print(f"{now():%H:%M:%S} {msg}", file=sys.stderr, flush=True)


# --------------------------------------------------------------- backend


class Backend:
    def __init__(self, ns, svc, port):
        self.pf = None
        url = os.environ.get("ND_BACKEND_URL")
        if not url:
            self.pf = subprocess.Popen(
                ["kubectl", "-n", ns, "port-forward", f"svc/{svc}", f"{port}:8080"],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            url = f"http://127.0.0.1:{port}"
        self.url = url.rstrip("/")
        for _ in range(30):
            try:
                self.get("/healthz")
                return
            except Exception:
                time.sleep(1)
        raise SystemExit(f"backend not reachable at {self.url}")

    def close(self):
        if self.pf:
            self.pf.terminate()

    def get(self, path):
        with urllib.request.urlopen(self.url + path, timeout=20) as r:
            return json.load(r)

    def post(self, path, body):
        req = urllib.request.Request(self.url + path, data=json.dumps(body).encode(),
                                     headers={"Content-Type": "application/json"}, method="POST")
        try:
            with urllib.request.urlopen(req, timeout=20) as r:
                return json.load(r)
        except urllib.error.HTTPError as e:
            raise RuntimeError(f"POST {path}: HTTP {e.code} {e.read().decode()[:300]}")

    def investigate(self, incident_id, arch, timeout_s=1200):
        """Start an eval run and wait for it. Returns the stored run."""
        run_id = self.post("/eval/runs", {"incident_id": incident_id, "arch": arch})["run_id"]
        t0 = time.time()
        end = t0 + timeout_s
        while time.time() < end:
            time.sleep(10)
            try:
                run = self.get(f"/eval/runs/{run_id}")
            except Exception:
                continue
            if run.get("status") in ("done", "failed"):
                run["wall_s"] = round(time.time() - t0)  # harness-side, 10s poll granularity
                return run
        return {"run_id": run_id, "status": "timeout", "incident_id": incident_id, "arch": arch}


# ----------------------------------------------------------------- scoring


def load_expectations():
    with open(os.path.join(HERE, "expectations.json")) as f:
        e = json.load(f)
    e.pop("_comment", None)
    return e


def expand(term, truth):
    """'{key}' -> truth values (list), plain text -> [text]."""
    m = re.fullmatch(r"\{(\w+)\}", term)
    if not m:
        return [term]
    v = truth.get(m.group(1))
    if v is None:
        return []
    return v if isinstance(v, list) else [v]


def mentions(text, term):
    # start-of-word match; terms may be prefixes ("retransmi").
    return re.search(r"(?<![a-z0-9])" + re.escape(term.lower()), text) is not None


def score(run, template, truth):
    o = run.get("outcome") or {}
    r = o.get("result") or {}
    cause = str(r.get("root_cause") or "").lower()
    routed = r.get("scenario") == template["scenario"]
    status_ok = r.get("investigation_status") in template["status"]
    groups = []
    for g in template["cause_all"]:
        alts = [a for t in g for a in expand(t, truth)]
        groups.append({"alternatives": alts, "hit": any(mentions(cause, a) for a in alts)})
    cause_ok = bool(groups) and all(g["hit"] for g in groups)
    checks = o.get("checks") or []
    return {
        "completed": run.get("status") == "done",
        "routed": routed,
        "status_ok": status_ok,
        "cause_ok": cause_ok,
        "correct": run.get("status") == "done" and routed and status_ok and cause_ok,
        "status": r.get("investigation_status"),
        "confidence": r.get("confidence"),
        "groups": groups,
        "tool_calls": o.get("tool_calls", 0),
        "holmes_calls": o.get("holmes_calls", 0),
        "duration_s": round((o.get("duration_ms") or 0) / 1000, 1) or run.get("wall_s", 0),
        "rechecked": bool(o.get("rechecked")),
        "check_error": o.get("check_error", ""),
        "jev_consistent": [c.get("consistent") for c in checks],
        "jev_tokens": sum(c.get("input_tokens", 0) for c in checks),
        "root_cause": str(r.get("root_cause") or o.get("error") or "")[:400],
    }


# ------------------------------------------------------------------ modes


def out_path(args, mode):
    os.makedirs(os.path.join(HERE, "results"), exist_ok=True)
    return args.out or os.path.join(HERE, "results", f"{mode}-{now():%Y%m%dT%H%M%SZ}.jsonl")


def write(fh, rec):
    fh.write(json.dumps(rec, ensure_ascii=False) + "\n")
    fh.flush()


def run_cases(be, cases, archs, repeat, fh, mode, expectations):
    jev_ok = True
    for rep in range(repeat):
        for c in cases:
            tmpl = expectations[c["expect"]]
            for arch in archs:
                if arch == "derived+jev" and not jev_ok:
                    continue
                log(f"[{mode} rep {rep + 1}/{repeat}] {c['id']} {c['incident_id']} {arch} ...")
                run = be.investigate(c["incident_id"], arch)
                s = score(run, tmpl, c["truth"])
                if arch == "derived+jev" and "not configured" in s["check_error"]:
                    log("derived+jev: JEV_API_KEY is not set on the backend; skipping that architecture")
                    jev_ok = False
                    continue
                write(fh, {"mode": mode, "rep": rep, "case": c["id"], "incident_id": c["incident_id"],
                           "expect": c["expect"], "hard": c.get("hard", ""), "arch": arch,
                           "run_id": run.get("run_id"), "score": s,
                           "at": now().isoformat(timespec="seconds") + "Z"})
                log(f"   -> {'CORRECT' if s['correct'] else 'wrong'} (routed={s['routed']} status={s['status']} "
                    f"cause={s['cause_ok']}) {s['duration_s']}s, {s['tool_calls']} tool calls")


def cmd_replay(args):
    with open(args.cases) as f:
        cases = json.load(f)["cases"]
    if args.only:
        keep = set(args.only.split(","))
        cases = [c for c in cases if c["id"] in keep or c["expect"] in keep]
    archs = args.archs.split(",")
    for a in archs:
        if a not in ARCHS:
            raise SystemExit(f"unknown arch {a}")
    path = out_path(args, "replay")
    log(f"{len(cases)} cases x {len(archs)} archs x {args.repeat} = {len(cases) * len(archs) * args.repeat} runs -> {path}")
    be = Backend(args.ns, args.svc, args.port)
    try:
        with open(path, "a") as fh:
            run_cases(be, cases, archs, args.repeat, fh, "replay", load_expectations())
    finally:
        be.close()
    print(path)


def read_truth(path):
    truth = {}
    if not os.path.exists(path):
        return truth
    with open(path) as f:
        for line in f:
            parts = line.rstrip("\n").split("\t")
            if len(parts) != 3:
                continue
            tmpl, key, val = parts
            truth.setdefault(tmpl, {}).setdefault(key, []).append(val)
    return truth


def cmd_live(args):
    script = os.path.abspath(args.script)
    rules = SCRIPT_RULES.get(os.path.basename(script))
    if not rules:
        raise SystemExit(f"unknown repro script {script}")
    archs = args.archs.split(",")
    tmp = tempfile.mkdtemp(prefix="nd-eval-")
    truth_file, hold_file = os.path.join(tmp, "truth.tsv"), os.path.join(tmp, "hold")
    open(hold_file, "w").close()
    env = dict(os.environ, ND_TRUTH_FILE=truth_file, ND_HOLD_FILE=hold_file,
               ND_DURATION=str(args.fault_min * 60), ND_WAIT_MIN=str(args.wait_min))
    since = now().strftime("%Y-%m-%dT%H:%M:%S")
    log(f"starting {os.path.basename(script)} (fault held up to {args.fault_min} min); log {tmp}/script.log")
    proc = subprocess.Popen(["bash", script], env=env, stdout=open(os.path.join(tmp, "script.log"), "w"),
                            stderr=subprocess.STDOUT, start_new_session=True)
    be = Backend(args.ns, args.svc, args.port + 1)
    path = out_path(args, "live")
    try:
        # Wait for the script's own wait to finish: incidents exist then.
        end = time.time() + (args.wait_min + 5) * 60
        found = {}
        while time.time() < end and proc.poll() is None:
            time.sleep(20)
            try:
                incs = be.get("/incidents")["incidents"]
            except Exception:
                continue
            for i in incs:
                member = i.get("holmes_status") == "grouped" or (
                    i.get("correlation_id") and i.get("correlation_id") != i["incident_id"])
                if i["starts_at"] >= since and i["rule_id"] in rules and not member and i["rule_id"] != "rule-5":
                    found.setdefault(i["rule_id"], i["incident_id"])
            if set(found) >= {r for r in rules if r != "rule-5"}:
                break
        if not found:
            raise SystemExit("no incident appeared; see the script log")
        truth = read_truth(truth_file)
        cases = []
        for rule, inc_id in sorted(found.items()):
            tmpl = next((t for t in truth if t.split("-cross")[0] == rule), rule)
            cases.append({"id": f"live-{tmpl}", "incident_id": inc_id, "expect": tmpl,
                          "truth": {k: v for k, v in truth.get(tmpl, {}).items()}})
        log(f"incidents: {found}; investigating with {archs} while the fault is live")
        with open(path, "a") as fh:
            run_cases(be, cases, archs, args.repeat, fh, "live", load_expectations())
    finally:
        try:
            os.remove(hold_file)
        except FileNotFoundError:
            pass
        try:
            proc.wait(timeout=120)
        except subprocess.TimeoutExpired:
            os.killpg(proc.pid, signal.SIGINT)  # the script's trap restores the cluster
            proc.wait(timeout=60)
        be.close()
    print(path)


# ------------------------------------------------------------------ report


def pct(n, d):
    return f"{100 * n / d:.0f}% ({n}/{d})" if d else "-"


def cmd_report(args):
    rows = []
    for p in args.files:
        with open(p) as f:
            rows += [json.loads(l) for l in f if l.strip()]
    if not rows:
        raise SystemExit("no results")
    archs = [a for a in ARCHS if any(r["arch"] == a for r in rows)]
    out = []
    out.append(f"# NetworkDoctor RCA evaluation\n\n{len(rows)} runs from {', '.join(os.path.basename(p) for p in args.files)}.\n")
    out.append("A run is **correct** when it routes to the expected skill, its status is acceptable, and its root cause "
               "names the injected fault (see eval/expectations.json).\n")
    out.append("## Summary\n")
    out.append("| Architecture | Correct | Routed | Status ok | Cause ok | Avg tool calls | Avg duration | Rechecked |")
    out.append("| --- | --- | --- | --- | --- | --- | --- | --- |")
    for a in archs:
        rs = [r["score"] for r in rows if r["arch"] == a]
        n = len(rs)
        out.append(f"| {a} | {pct(sum(s['correct'] for s in rs), n)} | {pct(sum(s['routed'] for s in rs), n)} | "
                   f"{pct(sum(s['status_ok'] for s in rs), n)} | {pct(sum(s['cause_ok'] for s in rs), n)} | "
                   f"{sum(s['tool_calls'] for s in rs) / n:.1f} | {sum(s['duration_s'] for s in rs) / n:.0f}s | "
                   f"{sum(s['rechecked'] for s in rs)} |")
    out.append("\n## By scenario\n")
    out.append("| Expectation | " + " | ".join(archs) + " |")
    out.append("| --- |" + " --- |" * len(archs))
    for e in sorted({r["expect"] for r in rows}):
        cells = []
        for a in archs:
            rs = [r["score"] for r in rows if r["expect"] == e and r["arch"] == a]
            cells.append(pct(sum(s["correct"] for s in rs), len(rs)))
        out.append(f"| {e} | " + " | ".join(cells) + " |")
    out.append("\n## Runs\n")
    out.append("| Case | Arch | Rep | Correct | Status | Cause hit | Tool calls | Root cause (start) |")
    out.append("| --- | --- | --- | --- | --- | --- | --- | --- |")
    for r in sorted(rows, key=lambda r: (r["case"], ARCHS.index(r["arch"]), r["rep"])):
        s = r["score"]
        rc = s["root_cause"].replace("|", "/").replace("\n", " ")[:140]
        out.append(f"| {r['case']}{' (hard)' if r.get('hard') else ''} | {r['arch']} | {r['rep'] + 1} | "
                   f"{'yes' if s['correct'] else 'no'} | {s['status']} | {'yes' if s['cause_ok'] else 'no'} | "
                   f"{s['tool_calls']} | {rc} |")
    print("\n".join(out))


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--ns", default=os.environ.get("ND_NS", "networkdoctor"))
    ap.add_argument("--svc", default=os.environ.get("ND_BACKEND_SVC", "networkdoctor-backend"))
    ap.add_argument("--port", type=int, default=18180)
    sub = ap.add_subparsers(dest="cmd", required=True)

    p = sub.add_parser("replay", help="re-investigate the stored corpus")
    p.add_argument("--cases", default=os.path.join(HERE, "cases.json"))
    p.add_argument("--archs", default="baseline,derived,derived+jev")
    p.add_argument("--repeat", type=int, default=1)
    p.add_argument("--only", help="comma-separated case ids or expectation names")
    p.add_argument("--out")
    p.set_defaults(fn=cmd_replay)

    p = sub.add_parser("live", help="inject one fault and investigate it with every architecture")
    p.add_argument("script")
    p.add_argument("--archs", default="baseline,derived,derived+jev")
    p.add_argument("--repeat", type=int, default=1)
    p.add_argument("--fault-min", type=int, default=45)
    p.add_argument("--wait-min", type=int, default=16)
    p.add_argument("--out")
    p.set_defaults(fn=cmd_live)

    p = sub.add_parser("report", help="score result files")
    p.add_argument("files", nargs="+")
    p.set_defaults(fn=cmd_report)

    args = ap.parse_args()
    args.fn(args)


if __name__ == "__main__":
    main()
