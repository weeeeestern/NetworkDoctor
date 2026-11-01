#!/usr/bin/env python3
"""Ask HolmesGPT the same live fault under different conditions (standard library only).

run.py compares backend architectures through POST /eval/runs. This script
skips the backend and asks Holmes /api/chat directly, so the question itself
can change: with or without the alert context, and against a Holmes with or
without the NetworkDoctor skills (scripts/repro/holmes-variant.sh).

Ask styles
  full     what the backend sends (holmes.BuildAsk): rule labels, node,
           investigation window, optional related alerts
  symptom  only an operator-style report about one node (--symptom)
  blind    "is there any network problem in the cluster?"

Every style carries BuildAsk's anti-plan line, because without it gpt-luna
ends about half of the direct asks with a plan and no investigation. An
answer counts as valid when it has a yaml block, is longer than 800
characters and used at least 5 tools; invalid answers are retried until each
arm has --target valid answers or --max-attempts runs. Arms take turns, so
none of them is favoured by how the fault evolves over time.

Example: CPU decoy, skills vs no skills, with and without context
  bash scripts/repro/holmes-variant.sh up raw
  python3 eval/ablation.py --script scripts/repro/t4-cpu-decoy.sh \\
    --endpoint main=holmesgpt/holmesgpt-holmes \\
    --endpoint raw=holmesgpt-raw/holmesgpt-raw-holmes \\
    --arm full_skill=full@main --arm full_raw=full@raw \\
    --arm symptom_skill=symptom@main --arm symptom_raw=symptom@raw \\
    --rule-id rule-1 --scenario network-congestion \\
    --alertname NetworkDoctorNetworkCongestion --target 4
  bash scripts/repro/holmes-variant.sh down raw

With --script and no --node, the node comes from the script's ground truth
(ND_TRUTH_FILE).

The answers are saved under eval/results/ablation-<time>/ (git-ignored). The
--flag counts are only hints; decide what each answer concluded by reading it.
"""

import argparse
import datetime as dt
import json
import os
import re
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))

ANTI = ("Do not reply with a plan or a status update: keep calling tools until the "
        "investigation is finished, and only then send one final reply that ends with the yaml block.")
SCHEMA = ("rule_id, scenario, investigation_status, confidence, time_window, affected_resources, "
          "root_cause, trigger_evidence, supporting_evidence, excluded_alternatives, "
          "recommended_actions, additional_checks")
SHORT_YAML = ("Finish with exactly one fenced ```yaml block "
              "(scenario, investigation_status, confidence, root_cause, recommended_actions).")
MIN_LEN, MIN_CALLS = 800, 5


def now():
    return dt.datetime.now(dt.timezone.utc).replace(microsecond=0)


def iso(t):
    return t.strftime("%Y-%m-%dT%H:%M:%SZ")


def log(msg):
    print(f"{now():%H:%M:%S} {msg}", file=sys.stderr, flush=True)


# ------------------------------------------------------------------ asks

def ask_full(a, at, started):
    lines = [
        "A NetworkDoctor alert fired. Investigate the root cause with the matching NetworkDoctor skill, "
        "Prometheus and read-only Kubernetes tools.",
        f"Reply with a short summary, then exactly one fenced ```yaml block that follows the skill's "
        f"Output Schema ({SCHEMA}).",
        ANTI,
        "",
        "incident_id: inc-ablation",
        f"cluster: {a.cluster}",
        "alert_status: firing",
        f"starts_at: {iso(started)}",
        f"investigation_window: {iso(at - dt.timedelta(minutes=15))} to {iso(at)}",
        "labels:",
    ]
    labels = {"alertname": a.alertname, "node": a.node, "rule_id": a.rule_id,
              "scenario": a.scenario, "severity": "warning"}
    lines += [f'  {k}: "{v}"' for k, v in sorted(labels.items()) if v]
    if a.related:
        lines += ["", "Other NetworkDoctor alerts started on the same node/service within the correlation "
                  "window. Decide whether they share one root cause with the alert above; if so, explain "
                  "the link in root_cause and list them under scenario_details.related_alerts, otherwise "
                  "say they are separate:"]
        for i, r in enumerate(a.related, 1):
            name, rule, scen, summary = (r.split("|") + ["", "", "", ""])[:4]
            lines.append(f'- incident inc-ablation-r{i}: alertname={name} rule_id={rule} scenario={scen} '
                         f'starts_at={iso(started)} summary="{summary}"')
    return "\n".join(lines) + "\n"


def ask_symptom(a, at, started):
    text = a.symptom or (f"An operator reports that node {a.node} has been degraded for the last several "
                         "minutes: requests involving that node are slow or intermittently failing.")
    return (f"{text}\nInvestigate and determine the root cause, using Prometheus and read-only Kubernetes "
            f"tools. Report what is actually causing the degradation and rule out what is not.\n{ANTI}\n{SHORT_YAML}")


def ask_blind(a, at, started):
    return ("Please check whether there is any network problem in the cluster right now, using Prometheus "
            f"and read-only Kubernetes tools.\n{ANTI}\n{SHORT_YAML}")


STYLES = {"full": ask_full, "symptom": ask_symptom, "blind": ask_blind}


# ------------------------------------------------------------- endpoints

def free_port(start):
    for p in range(start, start + 200):
        with socket.socket() as s:
            if s.connect_ex(("127.0.0.1", p)) != 0:
                return p
    raise SystemExit("no free local port")


class Endpoint:
    """A Holmes HTTP API, reached directly (http://...) or through kubectl port-forward (ns/svc)."""

    def __init__(self, name, target, port):
        self.name, self.pf = name, None
        if target.startswith("http"):
            self.url = target.rstrip("/")
            return
        ns, svc = target.split("/", 1)
        self.pf = subprocess.Popen(["kubectl", "-n", ns, "port-forward", f"svc/{svc}", f"{port}:80"],
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.url = f"http://127.0.0.1:{port}"
        for _ in range(30):
            with socket.socket() as s:
                if s.connect_ex(("127.0.0.1", port)) == 0:
                    return
            time.sleep(1)
        raise SystemExit(f"port-forward to {target} did not come up")

    def close(self):
        if self.pf:
            self.pf.terminate()

    def chat(self, ask, model, timeout_s=900):
        req = urllib.request.Request(self.url + "/api/chat", data=json.dumps({"ask": ask, "model": model}).encode(),
                                     headers={"Content-Type": "application/json"})
        t0 = time.time()
        with urllib.request.urlopen(req, timeout=timeout_s) as r:
            d = json.load(r)
        return d.get("analysis") or "", len(d.get("tool_calls") or []), round(time.time() - t0, 1)


# ------------------------------------------------------------------ run

def parse_pairs(items, what):
    out = {}
    for it in items:
        if "=" not in it:
            raise SystemExit(f"--{what} expects name=value, got {it!r}")
        k, v = it.split("=", 1)
        out[k] = v
    return out


def start_script(path, fault_min):
    tmp = tempfile.mkdtemp(prefix="nd-ablation-")
    hold, truth = os.path.join(tmp, "hold"), os.path.join(tmp, "truth.tsv")
    open(hold, "w").close()
    env = dict(os.environ, ND_HOLD_FILE=hold, ND_TRUTH_FILE=truth,
               ND_DURATION=str(fault_min * 60), ND_WAIT_MIN=str(fault_min))
    logf = os.path.join(tmp, "script.log")
    proc = subprocess.Popen(["bash", os.path.abspath(path)], env=env, stdout=open(logf, "w"),
                            stderr=subprocess.STDOUT, start_new_session=True)
    log(f"started {os.path.basename(path)} (fault held up to {fault_min} min); log {logf}")
    return proc, hold, truth, logf


def truth_node(path):
    """First node the repro script recorded as broken (nd_truth <tmpl> node <name>)."""
    try:
        with open(path) as f:
            for line in f:
                parts = line.rstrip("\n").split("\t")
                if len(parts) == 3 and parts[1] == "node":
                    return parts[2]
    except FileNotFoundError:
        pass
    return ""


def stop_script(proc, hold):
    try:
        os.remove(hold)
    except FileNotFoundError:
        pass
    if proc.poll() is None:
        os.killpg(proc.pid, signal.SIGINT)  # the script's EXIT trap restores the cluster
        try:
            proc.wait(timeout=120)
        except subprocess.TimeoutExpired:
            os.killpg(proc.pid, signal.SIGKILL)


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--endpoint", action="append", default=[], metavar="NAME=NS/SVC|URL", required=True)
    p.add_argument("--arm", action="append", default=[], metavar="NAME=STYLE@ENDPOINT", required=True)
    p.add_argument("--node", default="", help="node named in full/symptom asks")
    p.add_argument("--rule-id", default="")
    p.add_argument("--scenario", default="")
    p.add_argument("--alertname", default="")
    p.add_argument("--cluster", default="lab")
    p.add_argument("--related", action="append", default=[], metavar="ALERTNAME|RULE|SCENARIO|SUMMARY",
                   help="add a related alert to full asks (a correlated group)")
    p.add_argument("--symptom", default="", help="operator report used by symptom asks")
    p.add_argument("--script", help="repro script to run and hold while asking (uses ND_HOLD_FILE)")
    p.add_argument("--warmup-min", type=float, default=3, help="minutes to let the fault build before asking")
    p.add_argument("--fault-min", type=int, default=45,
                   help="max minutes from the script start: no ask starts after it, a running ask is cut at it, "
                        "then the hold is released")
    p.add_argument("--target", type=int, default=3, help="valid answers per arm")
    p.add_argument("--max-attempts", type=int, default=7, help="attempts per arm")
    p.add_argument("--model", default="gateway-luna")
    p.add_argument("--flag", action="append", default=[], metavar="NAME=REGEX",
                   help="count answers matching REGEX (case-insensitive); a hint, not a verdict")
    p.add_argument("--port-base", type=int, default=18100)
    a = p.parse_args()

    targets = parse_pairs(a.endpoint, "endpoint")
    arms = []
    for name, spec in parse_pairs(a.arm, "arm").items():
        style, _, ep = spec.partition("@")
        if style not in STYLES or ep not in targets:
            raise SystemExit(f"bad --arm {name}={spec}: style must be one of {sorted(STYLES)}, "
                             f"endpoint one of {sorted(targets)}")
        arms.append({"name": name, "style": style, "endpoint": ep, "valid": 0, "attempts": 0})
    flags = {k: re.compile(v, re.I) for k, v in parse_pairs(a.flag, "flag").items()}

    out = os.path.join(HERE, "results", f"ablation-{now():%Y%m%dT%H%M%SZ}")
    os.makedirs(os.path.join(out, "answers"))

    proc = hold = None
    endpoints = {}
    started = now() - dt.timedelta(minutes=5)
    deadline = None  # with --script: the fault must not outlive --fault-min
    try:
        if a.script:
            proc, hold, truth, logf = start_script(a.script, a.fault_min)
            started = now()
            deadline = time.time() + a.fault_min * 60
            end = time.time() + a.warmup_min * 60
            while time.time() < end:
                if proc.poll() is not None:
                    raise SystemExit(f"repro script exited early; see {logf}")
                time.sleep(5)
            if not a.node:
                a.node = truth_node(truth)
                log(f"node from ground truth: {a.node or '(none recorded)'}")
        with open(os.path.join(out, "config.json"), "w") as f:
            json.dump(vars(a), f, indent=1)
        port = a.port_base
        for name in sorted({arm["endpoint"] for arm in arms}):
            port = free_port(port)
            endpoints[name] = Endpoint(name, targets[name], port)
            port += 1

        with open(os.path.join(out, "attempts.jsonl"), "a") as fh:
            stop = False
            while not stop:
                todo = [arm for arm in arms if arm["valid"] < a.target and arm["attempts"] < a.max_attempts]
                if not todo:
                    break
                for arm in todo:  # one attempt per arm per round
                    if proc is not None and proc.poll() is not None:
                        raise SystemExit("repro script ended while asking; the fault is gone")
                    left = deadline - time.time() if deadline else 900
                    if left < 60:
                        log("--fault-min reached; no more asks, releasing the fault")
                        stop = True
                        break
                    arm["attempts"] += 1
                    at = now()
                    ask = STYLES[arm["style"]](a, at, started)
                    rec = {"arm": arm["name"], "style": arm["style"], "endpoint": arm["endpoint"],
                           "attempt": arm["attempts"], "asked_at": iso(at)}
                    try:
                        text, calls, wall = endpoints[arm["endpoint"]].chat(ask, a.model, timeout_s=min(900, left))
                    except Exception as e:  # keep going; a failed call is just an invalid attempt
                        rec.update(valid=False, error=str(e)[:300])
                        log(f"{arm['name']:14s} #{arm['attempts']} ERROR {e}")
                    else:
                        valid = bool(re.search(r"```ya?ml", text)) and len(text) > MIN_LEN and calls >= MIN_CALLS
                        fname = f"{arm['name']}-{arm['attempts']}.txt"
                        with open(os.path.join(out, "answers", fname), "w") as f:
                            f.write(f"--- ask ---\n{ask}\n--- answer ---\n{text}\n")
                        rec.update(valid=valid, wall_s=wall, tool_calls=calls, chars=len(text), file=fname,
                                   flags={k: bool(rx.search(text)) for k, rx in flags.items()})
                        arm["valid"] += valid
                        hits = " ".join(f"{k}={'y' if v else 'n'}" for k, v in rec["flags"].items())
                        log(f"{arm['name']:14s} #{arm['attempts']} {'valid' if valid else 'STUB '} "
                            f"{wall:6.1f}s calls={calls:3d} chars={len(text):5d} {hits}")
                    fh.write(json.dumps(rec, ensure_ascii=False) + "\n")
                    fh.flush()
    finally:
        for ep in endpoints.values():
            ep.close()
        if proc is not None:
            stop_script(proc, hold)

    print(f"\n{'arm':16s} {'valid/attempts':>14s}")
    for arm in arms:
        print(f"{arm['name']:16s} {arm['valid']:>6d}/{arm['attempts']:<7d}")
    print(f"\nanswers: {out}/answers  (read them to judge each conclusion)")


if __name__ == "__main__":
    main()
