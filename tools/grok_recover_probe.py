#!/usr/bin/env python3
"""Grok account recovery probe.

Design goals:
- Never full-scan the whole Grok pool.
- Only probe recovery queues:
  1) transport / token / proxy style temp failures  (fast queue)
  2) free-usage 429 whose rate_limit_reset_at has expired (fast queue)
  3) sticky entitlement 403 holds                     (slow queue)
  4) newer VPS active-probe 401/402/403 observations   (priority queue)
- Healthy / currently schedulable accounts are skipped.
- Successful probe/test path in backend clears sticky hold.
- Always call quota with mode=active so chat readiness, not billing, decides rescue.

Environment:
  SUB2API_BASE_URL   default http://127.0.0.1:6780
  SUB2API_JWT        optional; if empty, generated via go run ./cmd/jwtgen
  HOST_RUN_ENV       default <repo>/deploy/host-run.env
  GROK_RECOVER_CONCURRENCY default 4
  GROK_RECOVER_TIMEOUT_SEC default 45
  GROK_RECOVER_FAST_LIMIT  default 80
  GROK_RECOVER_403_LIMIT   default 120
  GROK_RECOVER_403_INTERVAL_SEC default 21600  (6h)
  GROK_RECOVER_FORCE_403   1 to force sticky queue this run
  GROK_RECOVER_VPS_OBSERVATION_ONLY 1 to process only new VPS failure observations
  GROK_RECOVER_VPS_OBSERVATION_LIMIT default 24
  GROK_PREFERRED_PROXY_ID  default 6; warn if Grok accounts drift away
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor, as_completed
from collections import Counter
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


def now_local() -> str:
    return datetime.now().strftime("%Y-%m-%d %H:%M:%S %z")


def log(msg: str) -> None:
    print(f"[{now_local()}] {msg}", flush=True)


def repo_root() -> Path:
    return Path(__file__).resolve().parents[1]


def load_env_file(path: Path) -> dict[str, str]:
    env: dict[str, str] = {}
    if not path.exists():
        return env
    for line in path.read_text(encoding="utf-8").splitlines():
        s = line.strip()
        if not s or s.startswith("#") or "=" not in s:
            continue
        k, v = s.split("=", 1)
        env[k.strip()] = v.strip()
    return env


def ensure_jwt(repo: Path, env_file: Path) -> str:
    jwt = os.environ.get("SUB2API_JWT", "").strip()
    if jwt:
        return jwt
    cmd = (
        "set -a; source "
        + str(env_file)
        + "; set +a; "
        + "cd "
        + str(repo / "backend")
        + '; go run ./cmd/jwtgen 2>/dev/null | awk -F= \'$1=="JWT"{print $2; exit}\''
    )
    out = subprocess.check_output(["/bin/zsh", "-lc", cmd], text=True).strip()
    if not out:
        raise RuntimeError("failed to generate admin JWT via jwtgen")
    return out


def psql_rows(env: dict[str, str], sql: str) -> list[list[str]]:
    host = env.get("DATABASE_HOST", "127.0.0.1")
    port = env.get("DATABASE_PORT", "5433")
    user = env.get("DATABASE_USER", "sub2api")
    db = env.get("DATABASE_DBNAME", "sub2api")
    password = env.get("DATABASE_PASSWORD", "")
    cmd = [
        "psql",
        f"postgresql://{user}@{host}:{port}/{db}",
        "-t",
        "-A",
        "-F",
        "\t",
        "-c",
        sql,
    ]
    out = subprocess.check_output(
        cmd,
        env={**os.environ, "PGPASSWORD": password},
        text=True,
    )
    rows: list[list[str]] = []
    for line in out.splitlines():
        if not line.strip():
            continue
        rows.append(line.split("\t"))
    return rows


def parse_ts(value: str | None) -> datetime | None:
    if not value:
        return None
    v = value.strip()
    if not v or v.lower() == "null":
        return None
    # postgres style / RFC3339
    v = v.replace(" ", "T")
    try:
        if v.endswith("+08"):
            v = v + ":00"
        if v.endswith("Z"):
            return datetime.fromisoformat(v.replace("Z", "+00:00"))
        return datetime.fromisoformat(v)
    except Exception:
        return None


def timestamp_epoch(value: datetime | None) -> float:
    if value is None:
        return 0.0
    if value.tzinfo is None:
        value = value.replace(tzinfo=timezone.utc)
    return value.timestamp()


def has_new_vps_failure_observation(item: dict[str, Any]) -> bool:
    """Return true only when a matched-version VPS failure is newer locally."""
    if item.get("local_credential_revision") != item.get("vps_credential_revision"):
        return False
    if item.get("vps_observation_source") != "active_probe":
        return False
    if item.get("vps_status_code") not in ("401", "402", "403"):
        return False
    return timestamp_epoch(item.get("vps_last_probe")) > timestamp_epoch(item.get("last_probe"))


def classify(http_code: int, body: str) -> tuple[str, int | None, str]:
    """Classify recovery probe results.

    Only chat/readiness outcomes count as rescue:
    - HTTP 200 with active/hybrid probe status_code 200 => ok_200
    - HTTP 200/429 with status_code 429 => rate_429
    - billing-only 200 without an active probe is NOT a rescue
    """
    try:
        obj = json.loads(body) if body else {}
    except Exception:
        obj = {}
    data = obj.get("data") if isinstance(obj, dict) else None
    if not isinstance(data, dict):
        data = {}
    source = str(data.get("source") or "").strip().lower()
    snap = data.get("snapshot") if isinstance(data.get("snapshot"), dict) else {}
    sc_raw = data.get("status_code")
    if sc_raw is None:
        sc_raw = snap.get("status_code")
    try:
        sc_i = int(sc_raw) if sc_raw is not None else None
    except Exception:
        sc_i = None
    probe_error = str(data.get("probe_error") or "")
    msg = (obj.get("message") or probe_error or body or "")[:240]
    reason = str(obj.get("reason") or "")

    if http_code == 200:
        # Recovery must come from chat readiness, never billing-only quota.
        if source in ("", "billing_probe") and not snap:
            return "billing_only", sc_i or 200, "billing-only quota response is not a chat readiness rescue"
        if probe_error:
            # Should not happen for hard failures after backend fix, but keep safe.
            low = probe_error.lower()
            if "403" in low or "permission-denied" in low or "entitlement" in low:
                return "forbidden_403", 403, probe_error[:240]
            if "401" in low or "token" in low or "oauth" in low or "refresh" in low:
                return "token_error", 401, probe_error[:240]
            if "429" in low or "rate" in low:
                return "rate_429", 429, probe_error[:240]
            return "other_probe_error", sc_i, probe_error[:240]
        if sc_i == 200:
            return "ok_200", 200, ""
        if sc_i == 429:
            return "rate_429", 429, ""
        if sc_i == 403:
            return "forbidden_403", 403, msg
        if sc_i == 402:
            return "payment_402", 402, msg
        if sc_i == 401:
            return "token_error", 401, msg
        if sc_i is None:
            # Active probe success always sets status_code; missing means unusable for recovery.
            return "unknown_status", None, "missing active probe status_code"
        return f"http200_sc_{sc_i}", sc_i, msg

    if http_code == 402 or sc_i == 402 or "upstream returned 402" in msg:
        return "payment_402", 402, msg
    if http_code == 403 or "permission-denied" in msg or "Access to the chat endpoint is denied" in msg or "entitlement" in msg.lower():
        return "forbidden_403", 403, msg
    if http_code == 429 or "free-usage-exhausted" in msg:
        return "rate_429", 429, msg
    if http_code == 401 or "token" in msg.lower() or "OAUTH" in reason or "refresh" in msg.lower() or "GROK_QUOTA_TOKEN" in msg:
        return "token_error", http_code or 401, msg
    if http_code in (502, 503, 504) or "EOF" in msg or "timeout" in msg.lower() or "transport" in msg.lower():
        return "transport_error", http_code or None, msg
    if http_code == 404 and "ACCOUNT_NOT_FOUND" in msg:
        return "account_missing", 404, msg
    return f"other_{http_code}", http_code or None, msg


def probe_one(base: str, jwt: str, account_id: int, timeout: int) -> dict[str, Any]:
    # mode=active forces chat readiness probe; billing-only quota is not recovery evidence.
    url = f"{base.rstrip('/')}/api/v1/admin/grok/accounts/{account_id}/quota?mode=active"
    req = urllib.request.Request(url, headers={"Authorization": f"Bearer {jwt}"})
    started = time.time()
    http_code = 0
    body = ""
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            http_code = resp.status
            body = resp.read().decode("utf-8", "ignore")
    except urllib.error.HTTPError as e:
        http_code = e.code
        try:
            body = e.read().decode("utf-8", "ignore")
        except Exception:
            body = str(e)
    except Exception as e:
        http_code = 0
        body = str(e)
    cls, sc, msg = classify(http_code, body)
    return {
        "id": account_id,
        "http_code": http_code,
        "class": cls,
        "status_code": sc,
        "message": msg,
        "latency_ms": int((time.time() - started) * 1000),
        "ts": datetime.now().isoformat(timespec="seconds"),
    }


def load_state(path: Path) -> dict[str, Any]:
    if not path.exists():
        return {}
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except Exception:
        return {}


def save_state(path: Path, state: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(state, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def main() -> int:
    repo = repo_root()
    env_file = Path(os.environ.get("HOST_RUN_ENV", repo / "deploy" / "host-run.env"))
    file_env = load_env_file(env_file)
    # expose for child tools if needed
    for k, v in file_env.items():
        os.environ.setdefault(k, v)

    base = os.environ.get("SUB2API_BASE_URL", f"http://{file_env.get('SERVER_HOST', '127.0.0.1')}:{file_env.get('SERVER_PORT', '6780')}")
    concurrency = int(os.environ.get("GROK_RECOVER_CONCURRENCY", "4"))
    timeout = int(os.environ.get("GROK_RECOVER_TIMEOUT_SEC", "45"))
    fast_limit = int(os.environ.get("GROK_RECOVER_FAST_LIMIT", "80"))
    sticky_limit = int(os.environ.get("GROK_RECOVER_403_LIMIT", "120"))
    sticky_interval = int(os.environ.get("GROK_RECOVER_403_INTERVAL_SEC", "21600"))
    force_403 = os.environ.get("GROK_RECOVER_FORCE_403", "0") in ("1", "true", "TRUE", "yes")
    vps_observation_only = os.environ.get("GROK_RECOVER_VPS_OBSERVATION_ONLY", "0") in ("1", "true", "TRUE", "yes")
    vps_observation_limit = int(os.environ.get("GROK_RECOVER_VPS_OBSERVATION_LIMIT", "24"))
    preferred_proxy = int(os.environ.get("GROK_PREFERRED_PROXY_ID", "6"))

    out_dir = Path(os.environ.get("GROK_RECOVER_OUT_DIR", repo / "deploy" / "data-host" / "grok-scan" / "recover"))
    out_dir.mkdir(parents=True, exist_ok=True)
    state_path = out_dir / "state.json"
    state = load_state(state_path)

    # health check
    try:
        with urllib.request.urlopen(base.rstrip("/") + "/", timeout=5) as resp:
            if resp.status >= 500:
                log(f"sub2api unhealthy status={resp.status}")
                return 2
    except Exception as e:
        log(f"sub2api unreachable at {base}: {e}")
        return 2

    jwt = ensure_jwt(repo, env_file)

    # proxy drift warning
    drift = psql_rows(
        file_env,
        f"""
        SELECT count(*) FROM accounts
        WHERE platform='grok' AND deleted_at IS NULL
          AND (proxy_id IS DISTINCT FROM {preferred_proxy})
        """,
    )
    drift_n = int(drift[0][0]) if drift else 0
    if drift_n:
        log(f"WARN: {drift_n} grok accounts not on preferred proxy #{preferred_proxy}")

    # candidate selection SQL
    # Fast queue:
    # - transport/token-ish temp reasons
    # - expired free-usage rate limits
    # Sticky queue:
    # - grok_hold_until_success true, or temp reason entitlement denied / snap 403 with hold
    sql = r"""
    SELECT
      id,
      name,
      status,
      COALESCE(proxy_id, 0)::text AS proxy_id,
      COALESCE((extra->>'grok_hold_until_success')::boolean, false)::text AS hold,
      COALESCE(extra->'grok_usage_snapshot'->>'status_code', '') AS snap,
      COALESCE(temp_unschedulable_reason, '') AS temp_reason,
      COALESCE(temp_unschedulable_until::text, '') AS temp_until,
      COALESCE(rate_limit_reset_at::text, '') AS rate_reset,
      COALESCE(extra->>'grok_free_usage_exhausted', '') AS free_exh,
      COALESCE(extra->'grok_usage_snapshot'->>'updated_at', '') AS snap_updated,
      COALESCE(extra->'grok_usage_snapshot'->>'last_probe_at', '') AS last_probe,
      md5(credentials::text) AS local_credential_revision,
      COALESCE(extra->'grok_vps_probe'->>'credential_revision', '') AS vps_credential_revision,
      COALESCE(extra->'grok_vps_probe'->>'status_code', '') AS vps_status_code,
      COALESCE(extra->'grok_vps_probe'->>'observation_source', '') AS vps_observation_source,
      COALESCE(extra->'grok_vps_probe'->>'last_probe_at', '') AS vps_last_probe
    FROM accounts
    WHERE platform = 'grok'
      AND deleted_at IS NULL
      AND (
        (
          lower(name) NOT LIKE '%@hotmail.%'
          AND lower(name) NOT LIKE '%@outlook.%'
          AND lower(name) NOT LIKE '%@live.%'
        )
        OR extra ? 'grok_vps_probe'
      )
    ORDER BY id
    """
    rows = psql_rows(file_env, sql)
    now = datetime.now(timezone.utc)

    vps_observations: list[dict[str, Any]] = []
    fast: list[dict[str, Any]] = []
    sticky: list[dict[str, Any]] = []

    for r in rows:
        item = {
            "id": int(r[0]),
            "name": r[1],
            "status": r[2],
            "proxy_id": int(r[3] or 0),
            "hold": (r[4] or "").lower() in ("t", "true", "1"),
            "snap": r[5] or "",
            "temp_reason": r[6] or "",
            "temp_until": parse_ts(r[7]),
            "rate_reset": parse_ts(r[8]),
            "free_exh": (r[9] or "").lower() in ("t", "true", "1"),
            "snap_updated": parse_ts(r[10]),
            "last_probe": parse_ts(r[11]),
            "local_credential_revision": r[12] or "",
            "vps_credential_revision": r[13] or "",
            "vps_status_code": r[14] or "",
            "vps_observation_source": r[15] or "",
            "vps_last_probe": parse_ts(r[16]),
        }
        if has_new_vps_failure_observation(item):
            item["queue"] = "vps_observation"
            vps_observations.append(item)
            continue
        if vps_observation_only:
            continue
        reason_l = item["temp_reason"].lower()
        temp_active = item["temp_until"] is not None and item["temp_until"] > now
        rate_active = item["rate_reset"] is not None and item["rate_reset"] > now

        transportish = any(
            k in reason_l
            for k in (
                "transport",
                "eof",
                "ssl",
                "proxy",
                "token refresh",
                "oauth",
                "unauthorized",
            )
        )
        # expired free usage: was exhausted, reset already passed, still not clearly healthy
        free_due = item["free_exh"] and item["rate_reset"] is not None and item["rate_reset"] <= now
        rate_due = item["rate_reset"] is not None and item["rate_reset"] <= now and item["snap"] == "429"

        if item["hold"] or (item["snap"] == "403" and (temp_active or "entitlement" in reason_l or "subscription tier" in reason_l)):
            item["queue"] = "sticky"
            sticky.append(item)
            continue

        if transportish or free_due or rate_due:
            # skip if currently healthy schedulable-ish already (no temp, no rate)
            if item["status"] == "active" and not temp_active and not rate_active and item["snap"] == "200" and not item["hold"]:
                continue
            item["queue"] = "fast"
            fast.append(item)

    # order: older probe first
    def age_key(it: dict[str, Any]) -> float:
        ts = it.get("last_probe") or it.get("snap_updated")
        if not ts:
            return 0.0
        try:
            return ts.timestamp()
        except Exception:
            return 0.0

    vps_observations.sort(key=lambda it: timestamp_epoch(it.get("vps_last_probe")), reverse=True)
    fast.sort(key=age_key)
    sticky.sort(key=age_key)

    last_403 = float(state.get("last_403_pass_epoch") or 0)
    due_403 = not vps_observation_only and (force_403 or (time.time() - last_403 >= sticky_interval))
    selected: list[dict[str, Any]] = []
    selected.extend(vps_observations[:vps_observation_limit])
    if vps_observation_only:
        log(
            f"queues: vps_observation={min(len(vps_observations), vps_observation_limit)}/{len(vps_observations)} only"
        )
    else:
        selected.extend(fast[:fast_limit])
        if due_403:
            selected.extend(sticky[:sticky_limit])
            log(
                f"queues: vps_observation={min(len(vps_observations), vps_observation_limit)}/{len(vps_observations)} fast={min(len(fast), fast_limit)}/{len(fast)} sticky={min(len(sticky), sticky_limit)}/{len(sticky)} (403 due)"
            )
        else:
            remain = int(sticky_interval - (time.time() - last_403))
            log(
                f"queues: vps_observation={min(len(vps_observations), vps_observation_limit)}/{len(vps_observations)} fast={min(len(fast), fast_limit)}/{len(fast)} sticky=0/{len(sticky)} (403 next in ~{max(remain,0)//60}m)"
            )

    # de-dup by id preserving order
    seen: set[int] = set()
    uniq: list[dict[str, Any]] = []
    for it in selected:
        if it["id"] in seen:
            continue
        seen.add(it["id"])
        uniq.append(it)
    selected = uniq

    if not selected:
        log("no recovery candidates")
        state["last_run_epoch"] = time.time()
        state["last_run_at"] = datetime.now().isoformat(timespec="seconds")
        state["last_result"] = {"scanned": 0, "rescued": 0}
        save_state(state_path, state)
        return 0

    stamp = datetime.now().strftime("%Y%m%d_%H%M%S")
    result_path = out_dir / f"recover_{stamp}.jsonl"
    summary_path = out_dir / f"recover_{stamp}.summary.json"
    latest_summary = out_dir / "latest.summary.json"

    log(f"probing n={len(selected)} concurrency={concurrency} base={base}")
    counts: Counter[str] = Counter()
    results: list[dict[str, Any]] = []
    lock = threading.Lock()
    done = 0
    t0 = time.time()

    def work(it: dict[str, Any]) -> dict[str, Any]:
        res = probe_one(base, jwt, it["id"], timeout)
        res["name"] = it["name"]
        res["queue"] = it.get("queue", "fast")
        res["prev_snap"] = it.get("snap")
        res["prev_reason"] = (it.get("temp_reason") or "")[:160]
        return res

    with result_path.open("w", encoding="utf-8") as outf, ThreadPoolExecutor(max_workers=concurrency) as ex:
        futs = {ex.submit(work, it): it for it in selected}
        for fut in as_completed(futs):
            item = fut.result()
            with lock:
                done += 1
                results.append(item)
                counts[item["class"]] += 1
                outf.write(json.dumps(item, ensure_ascii=False) + "\n")
                outf.flush()
                if done % 20 == 0 or done == len(selected):
                    log(f"progress {done}/{len(selected)} {dict(counts)}")

    rescued = [r for r in results if r["class"] in ("ok_200", "rate_429")]
    summary = {
        "scanned": len(results),
        "elapsed_sec": round(time.time() - t0, 1),
        "counts": dict(counts),
        "rescued_non403": len(rescued),
        "ok_200": [{"id": r["id"], "name": r["name"]} for r in sorted(results, key=lambda x: x["id"]) if r["class"] == "ok_200"],
        "rate_429": [{"id": r["id"], "name": r["name"]} for r in sorted(results, key=lambda x: x["id"]) if r["class"] == "rate_429"],
        "proxy_drift": drift_n,
        "preferred_proxy": preferred_proxy,
        "sticky_queue_ran": due_403,
        "vps_observation_only": vps_observation_only,
        "vps_observation_candidates": len(vps_observations),
        "fast_candidates": len(fast),
        "sticky_candidates": len(sticky),
        "result_file": str(result_path),
    }
    summary_path.write_text(json.dumps(summary, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    latest_summary.write_text(json.dumps(summary, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")

    state["last_run_epoch"] = time.time()
    state["last_run_at"] = datetime.now().isoformat(timespec="seconds")
    state["last_result"] = {
        "scanned": summary["scanned"],
        "rescued_non403": summary["rescued_non403"],
        "counts": summary["counts"],
        "summary_file": str(summary_path),
    }
    if due_403:
        state["last_403_pass_epoch"] = time.time()
        state["last_403_pass_at"] = datetime.now().isoformat(timespec="seconds")
    save_state(state_path, state)

    log(
        "done "
        + json.dumps(
            {
                "scanned": summary["scanned"],
                "rescued_non403": summary["rescued_non403"],
                "ok_200": len(summary["ok_200"]),
                "rate_429": len(summary["rate_429"]),
                "counts": summary["counts"],
            },
            ensure_ascii=False,
        )
    )
    if summary["ok_200"]:
        log("rescued 200 ids=" + ",".join(str(x["id"]) for x in summary["ok_200"]))
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except KeyboardInterrupt:
        raise SystemExit(130)
    except Exception as e:
        log(f"FATAL: {e}")
        raise
