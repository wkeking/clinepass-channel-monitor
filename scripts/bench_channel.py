#!/usr/bin/env python3
"""Benchmark the CPA streaming path to the Cline gateway model `deepseek-flash-1`.

What it measures, per run: prompt_tokens / completion_tokens from the streamed
`usage` object, number of SSE data frames, number of frames carrying visible
text, wall seconds, TTFT (seconds to the first text-bearing frame), the stream
window (first text frame -> last text frame), p50/p90 of the gap between
consecutive text frames in ms, decode throughput (completion_tokens / window),
output text characters, and the CPU cost of the `cpa` docker container during
the run (median / mean / max percent, plus an idle median sampled just before).

Prints one JSON object per run, then one JSON summary object with the median
across runs of p50 gap ms, tps, ttft s, wall s, cpu median and cpu max.

Run as root on the host that runs the `cpa` container (copy it there first; the
oracle keeps its copy at /tmp/bench_channel.py):

    sudo python3 bench_channel.py --label NAME --ctx-chars N \
        --max-tokens N --runs N

The CPA client key is read from /opt/cpa/config.yaml under access -> api-keys ->
[0] and is never printed. Exit code is 0 only when every run produced a usable
completion (non-zero completion_tokens); a failing run is reported clearly and
does not discard the results of the other runs.
"""

from __future__ import annotations

import argparse
import json
import math
import os
import re
import statistics
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request

CONFIG_PATH = "/opt/cpa/config.yaml"
ENDPOINT = "http://127.0.0.1:8317/v1/chat/completions"
MODEL = "deepseek-flash-1"
CONTAINER = "cpa"
TOTAL_TIMEOUT_S = 900.0
IDLE_SAMPLE_S = 5.0
CPU_SAMPLE_INTERVAL_S = 1.0
ERROR_BODY_CHARS = 300
REDACT_RE = re.compile(r"sk[-_][A-Za-z0-9_\-]{4,}")

# Context filler: same shape as /tmp/perf_probe.py. The %08d counter keeps every
# repetition distinct so an upstream prefix cache cannot collapse the prompt.
FILLER_UNIT = ("Observability in a distributed gateway rests on three signals: metrics, logs and traces. "
               "This paragraph exists to fill the context window; paragraph number %08d keeps every "
               "section distinct so the upstream does not collapse the prefix. ")
FILLER_QUESTION = ("\n\nExplain the trade-off between consistency and availability in distributed "
                   "systems in about 1200 tokens.")


def redact(text: str) -> str:
    """Mask anything that looks like an API key before it reaches stdout."""
    return REDACT_RE.sub("sk_***REDACTED***", text)


def _r(value, digits: int = 3):
    return None if value is None else round(value, digits)


def percentile(values, p: float):
    """Linear-interpolated percentile (p in 0..100); None for an empty series."""
    if not values:
        return None
    ordered = sorted(values)
    if len(ordered) == 1:
        return ordered[0]
    rank = (p / 100.0) * (len(ordered) - 1)
    lo = math.floor(rank)
    hi = math.ceil(rank)
    if lo == hi:
        return ordered[lo]
    return ordered[lo] + (ordered[hi] - ordered[lo]) * (rank - lo)


def _scan_key_fallback(path: str) -> str:
    """Minimal parser for the `access:` -> `api-keys:` -> [0] list, used when PyYAML is absent."""
    key = None
    in_access = False
    in_list = False
    with open(path, encoding="utf-8") as handle:
        for raw in handle:
            line = raw.rstrip("\n")
            if not line.strip() or line.lstrip().startswith("#"):
                continue
            body = line.strip()
            indent = len(line) - len(line.lstrip(" "))
            if indent == 0:
                in_access = body.startswith("access:")
                in_list = False
                continue
            if not in_access:
                continue
            if not in_list:
                if body.startswith("api-keys:"):
                    after = body.split(":", 1)[1].strip()
                    if after.startswith("["):
                        for item in after.strip("[]").split(","):
                            item = item.strip().strip("'\"")
                            if item:
                                key = item
                                break
                        in_list = True
                    elif after:
                        key = after.strip("'\"")
                    else:
                        in_list = True
                continue
            if body.startswith("-"):
                key = body[1:].strip().strip("'\"")
                break
            if not body.startswith("#"):
                in_list = False
    if not key:
        raise SystemExit("FATAL: no client api key at access -> api-keys -> [0] in %s" % path)
    return key


def load_client_key(path: str = CONFIG_PATH) -> str:
    try:
        with open(path, encoding="utf-8"):
            pass
    except OSError as exc:
        raise SystemExit("FATAL: cannot read %s (%s) - run this script as root" % (path, exc))
    key = None
    try:
        import yaml  # type: ignore

        with open(path, encoding="utf-8") as handle:
            cfg = yaml.safe_load(handle) or {}
        keys = ((cfg.get("access") or {}).get("api-keys")) or []
        if keys and isinstance(keys[0], str) and keys[0].strip():
            key = keys[0].strip()
    except ImportError:
        key = None
    if not key:
        key = _scan_key_fallback(path)
    return key


def build_prompt(ctx_chars: int) -> str:
    text = "".join(FILLER_UNIT % i for i in range(ctx_chars // len(FILLER_UNIT) + 1))[:ctx_chars]
    return text + FILLER_QUESTION


def cpu_sample_once():
    """One `docker stats` read: (cpu_percent, mem_usage) for the CPA container, or None."""
    try:
        proc = subprocess.run(
            ["docker", "stats", "--no-stream", "--format", "{{.Name}} {{.CPUPerc}} {{.MemUsage}}"],
            capture_output=True, text=True, timeout=60,
        )
    except Exception:
        return None
    for line in proc.stdout.splitlines():
        parts = line.split()
        if len(parts) >= 2 and parts[0] == CONTAINER:
            try:
                cpu = float(parts[1].rstrip("%"))
            except ValueError:
                return None
            return cpu, " ".join(parts[2:])
    return None


class CpuSampler(threading.Thread):
    """Samples the CPA container's CPU once per second while a run is in flight."""

    def __init__(self) -> None:
        super().__init__(daemon=True)
        self.stop_event = threading.Event()
        self.samples: list = []  # (issued_ts, done_ts, cpu_pct, mem_usage)
        self._lock = threading.Lock()

    def run(self) -> None:
        while not self.stop_event.is_set():
            started = time.time()
            got = cpu_sample_once()
            stamp = time.time()
            if got is not None:
                with self._lock:
                    self.samples.append((started, stamp, got[0], got[1]))
            self.stop_event.wait(max(0.0, CPU_SAMPLE_INTERVAL_S - (time.time() - started)))

    def snapshot(self) -> list:
        with self._lock:
            return list(self.samples)


def idle_cpu_median(seconds: float = IDLE_SAMPLE_S):
    """Median CPA CPU percent over an idle window of `seconds`, sampled ~1/s.

    Returns (median_percent, sample_count); median is None when no sample landed.
    """
    deadline = time.time() + seconds
    values = []
    while time.time() < deadline:
        started = time.time()
        got = cpu_sample_once()
        if got is not None:
            values.append(got[0])
        time.sleep(max(0.0, CPU_SAMPLE_INTERVAL_S - (time.time() - started)))
    return (statistics.median(values) if values else None), len(values)


def classify_silent_frame(obj: dict) -> str:
    """Why a frame carried no visible text - used to surface unusual stream shapes."""
    choices = obj.get("choices") or []
    has_usage = bool(obj.get("usage"))
    if not choices:
        return "usage_only" if has_usage else "no_choices"
    choice = choices[0]
    delta = choice.get("delta") or {}
    if not delta:
        return "finish_reason_only" if choice.get("finish_reason") else "empty_delta"
    if (delta.get("role") or "") and not any(delta.get(k) for k in ("content", "reasoning", "reasoning_content")):
        return "role_only"
    return "empty_delta"


def http_failure(kind: str, status, body: str) -> dict:
    """Normalise a transport-level failure into a redacted, truncated record."""
    text = redact((body or "").strip())[:ERROR_BODY_CHARS]
    return {"kind": kind, "status": status, "body": text}


def run_once(args, client_key: str, prompt: str, index: int, idle: tuple) -> dict:
    idle_cpu, idle_samples = idle
    body = {
        "model": MODEL,
        "stream": True,
        "stream_options": {"include_usage": True},
        "max_tokens": args.max_tokens,
        "temperature": 0.2,
        "messages": [{"role": "user", "content": prompt}],
    }
    request = urllib.request.Request(
        ENDPOINT,
        data=json.dumps(body).encode("utf-8"),
        headers={
            "Authorization": "Bearer " + client_key,
            "Content-Type": "application/json",
            "Accept": "text/event-stream",
        },
    )

    frame_stamps: list = []
    sse_frames = 0
    text_frames = 0
    text_chars = 0
    malformed_frames = 0
    usage = None
    done_seen = False
    silent_kinds: dict = {}
    failure = None

    sampler = CpuSampler()
    t0 = time.perf_counter()
    run_start_wall = time.time()
    first_text = None
    sampler.start()
    try:
        with urllib.request.urlopen(request, timeout=TOTAL_TIMEOUT_S) as response:
            for raw in response:
                now = time.perf_counter()
                line = raw.decode("utf-8", "replace").strip()
                if not line.startswith("data:"):
                    continue
                payload = line[5:].strip()
                if not payload:
                    continue
                sse_frames += 1
                if payload == "[DONE]":
                    done_seen = True
                    break
                try:
                    obj = json.loads(payload)
                except json.JSONDecodeError:
                    malformed_frames += 1
                    continue
                if obj.get("usage"):
                    usage = obj["usage"]
                choices = obj.get("choices") or []
                delta = (choices[0].get("delta") or {}) if choices else {}
                text = ((delta.get("content") or "") + (delta.get("reasoning") or "")
                        + (delta.get("reasoning_content") or ""))
                if text:
                    text_frames += 1
                    text_chars += len(text)
                    if first_text is None:
                        first_text = now
                    frame_stamps.append(now)
                else:
                    kind = classify_silent_frame(obj)
                    silent_kinds[kind] = silent_kinds.get(kind, 0) + 1
    except urllib.error.HTTPError as exc:
        raw_body = exc.read()
        failure = http_failure("HTTPError", exc.code, raw_body.decode("utf-8", "replace"))
    except urllib.error.URLError as exc:
        failure = http_failure("URLError", None, str(exc.reason))
    except Exception as exc:  # timeouts, resets, mid-stream aborts
        failure = http_failure(type(exc).__name__, None, str(exc))
    finally:
        t_end = time.perf_counter()
        run_end_wall = time.time()
        sampler.stop_event.set()
        sampler.join(timeout=30)

    gaps_ms = [(b - a) * 1000.0 for a, b in zip(frame_stamps, frame_stamps[1:])]
    completion = int((usage or {}).get("completion_tokens") or 0)
    prompt_tokens = (usage or {}).get("prompt_tokens")
    window = (frame_stamps[-1] - frame_stamps[0]) if len(frame_stamps) > 1 else 0.0
    tps = (completion / window) if window > 0 and completion else 0.0
    samples = sampler.snapshot()
    # A `docker stats` read reports the CPU rate over the interval since the previous
    # read, so a sample counts for the run when that interval [issued, done] overlaps
    # [run_start, run_end]; samples wholly outside the run are dropped.
    in_window = [(c, m) for issued, done, c, m in samples
                 if issued <= run_end_wall and done >= run_start_wall]
    cpu_values = [c for c, _ in in_window]
    max_sample = max(in_window, key=lambda item: item[0]) if in_window else None

    ok = failure is None and completion > 0
    record = {
        "event": "run",
        "label": args.label,
        "run": index,
        "ok": ok,
        "model": MODEL,
        "ctx_chars": args.ctx_chars,
        "prompt_chars": len(prompt),
        "max_tokens": args.max_tokens,
        "prompt_tokens": prompt_tokens,
        "completion_tokens": completion,
        "sse_frames": sse_frames,
        "text_frames": text_frames,
        "text_chars": text_chars,
        "silent_frames": sse_frames - text_frames - (1 if done_seen else 0) - malformed_frames,
        "silent_frame_kinds": silent_kinds,
        "malformed_frames": malformed_frames,
        "done_seen": done_seen,
        "usage_seen": usage is not None,
        "wall_s": _r(t_end - t0),
        "ttft_s": _r(first_text - t0 if first_text is not None else None),
        "window_s": _r(window),
        "p50_gap_ms": _r(percentile(gaps_ms, 50)),
        "p90_gap_ms": _r(percentile(gaps_ms, 90)),
        "tps": _r(tps, 2),
        "cpu_idle_median_pct": _r(idle_cpu, 2),
        "cpu_idle_samples": idle_samples,
        "cpu_median_pct": _r(statistics.median(cpu_values), 2) if cpu_values else None,
        "cpu_mean_pct": _r(statistics.fmean(cpu_values), 2) if cpu_values else None,
        "cpu_max_pct": _r(max(cpu_values), 2) if cpu_values else None,
        "cpu_mem_at_max": max_sample[1] if max_sample else None,
        "cpu_samples_total": len(samples),
        "cpu_samples_in_window": len(cpu_values),
    }
    if failure is not None:
        record["error"] = failure
        print("FAILURE label=%s run=%d status=%s kind=%s body=%s"
              % (args.label, index, failure["status"], failure["kind"], failure["body"]), flush=True)
    return record


def median_of(records, key: str, digits: int = 3):
    values = [r[key] for r in records if isinstance(r.get(key), (int, float))]
    return _r(statistics.median(values), digits) if values else None


def main() -> int:
    parser = argparse.ArgumentParser(description="Benchmark CPA streaming (deepseek-flash-1).")
    parser.add_argument("--label", default="unlabelled", help="tag written into every JSON line")
    parser.add_argument("--ctx-chars", type=int, default=227000,
                        help="synthetic context size in characters (~56k prompt tokens at 227000)")
    parser.add_argument("--max-tokens", type=int, default=1600)
    parser.add_argument("--runs", type=int, default=1)
    args = parser.parse_args()
    if args.runs < 1 or args.ctx_chars < 1 or args.max_tokens < 1:
        raise SystemExit("FATAL: --runs, --ctx-chars and --max-tokens must be >= 1")
    if os.geteuid() != 0:
        print("WARNING: not running as root; reading %s and docker stats may fail" % CONFIG_PATH,
              file=sys.stderr, flush=True)

    client_key = load_client_key()
    prompt = build_prompt(args.ctx_chars)
    print(json.dumps({"event": "config", "label": args.label, "endpoint": ENDPOINT, "model": MODEL,
                      "ctx_chars": args.ctx_chars, "prompt_chars": len(prompt),
                      "max_tokens": args.max_tokens, "runs": args.runs,
                      "temperature": 0.2, "container": CONTAINER}), flush=True)

    results = []
    for index in range(1, args.runs + 1):
        idle = idle_cpu_median()
        record = run_once(args, client_key, prompt, index, idle)
        results.append(record)
        print(json.dumps(record, ensure_ascii=False), flush=True)

    good = [r for r in results if r["ok"]]
    summary = {
        "event": "summary",
        "label": args.label,
        "model": MODEL,
        "ctx_chars": args.ctx_chars,
        "max_tokens": args.max_tokens,
        "runs": len(results),
        "ok_runs": len(good),
        "failed_runs": len(results) - len(good),
        "p50_gap_ms_median": median_of(good, "p50_gap_ms"),
        "p90_gap_ms_median": median_of(good, "p90_gap_ms"),
        "tps_median": median_of(good, "tps", 2),
        "ttft_s_median": median_of(good, "ttft_s"),
        "wall_s_median": median_of(good, "wall_s"),
        "window_s_median": median_of(good, "window_s"),
        "cpu_idle_median_pct_median": median_of(good, "cpu_idle_median_pct", 2),
        "cpu_median_pct_median": median_of(good, "cpu_median_pct", 2),
        "cpu_max_pct_median": median_of(good, "cpu_max_pct", 2),
        "prompt_tokens_median": median_of(good, "prompt_tokens", 0),
        "completion_tokens_median": median_of(good, "completion_tokens", 0),
    }
    print(json.dumps(summary, ensure_ascii=False), flush=True)

    usable = len(good) == len(results) and bool(results)
    print("RESULT label=%s ok=%d/%d usable=%s" % (args.label, len(good), len(results), usable), flush=True)
    return 0 if usable else 1


if __name__ == "__main__":
    sys.exit(main())
