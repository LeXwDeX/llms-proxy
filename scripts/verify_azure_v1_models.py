#!/usr/bin/env python3
import argparse
import json
import subprocess
import sys
import time
import urllib.parse
from collections import defaultdict
from pathlib import Path


def load_config(path: Path) -> dict:
    try:
        return json.loads(path.read_text())
    except Exception as exc:
        raise SystemExit(f"failed to read config {path}: {exc}")


def probe(target_name: str, endpoint: str, api_key: str, model: str, connect_timeout: int, max_time: int):
    url = f"{endpoint.rstrip('/')}/openai/v1/models/{urllib.parse.quote(model, safe='')}"
    cmd = [
        "curl",
        "-sS",
        "-o",
        "/tmp/azure_model_probe_body.txt",
        "-w",
        "%{http_code}",
        "--connect-timeout",
        str(connect_timeout),
        "--max-time",
        str(max_time),
        "-H",
        f"api-key: {api_key}",
        url,
    ]

    start = time.time()
    proc = subprocess.run(cmd, text=True, capture_output=True)
    elapsed_ms = int((time.time() - start) * 1000)

    if proc.returncode == 0:
        status = proc.stdout.strip() or "000"
        ok = status == "200"
        detail = f"http_{status}"
    else:
        status = f"curl_err_{proc.returncode}"
        ok = False
        detail = (proc.stderr or "").strip().split("\n")[-1][:200]

    return {
        "target": target_name,
        "model": model,
        "status": status,
        "ok": ok,
        "elapsed_ms": elapsed_ms,
        "detail": detail,
    }


def main() -> int:
    parser = argparse.ArgumentParser(description="Verify Azure OpenAI v1 model availability by config")
    parser.add_argument("--config", default="config/config.json", help="Path to config json")
    parser.add_argument("--connect-timeout", type=int, default=3, help="curl connect timeout seconds")
    parser.add_argument("--max-time", type=int, default=5, help="curl max time seconds")
    args = parser.parse_args()

    config_path = Path(args.config)
    cfg = load_config(config_path)

    rows = []
    for target in cfg.get("azure_targets", []):
        tname = (target.get("name") or "").strip()
        endpoint = (target.get("endpoint") or "").strip()
        api_key = target.get("azure_api_key") or ""
        models = target.get("allowed_models") or []

        for model in models:
            model = (model or "").strip()
            if not model:
                continue
            rows.append(probe(tname, endpoint, api_key, model, args.connect_timeout, args.max_time))

    summary = defaultdict(lambda: {"ok": 0, "fail": 0})
    for row in rows:
        if row["ok"]:
            summary[row["target"]]["ok"] += 1
        else:
            summary[row["target"]]["fail"] += 1

    print("=== SUMMARY ===")
    for target in sorted(summary):
        item = summary[target]
        print(f"{target}: ok={item['ok']} fail={item['fail']}")

    print("\n=== DETAILS ===")
    for row in rows:
        mark = "OK" if row["ok"] else "FAIL"
        print(f"[{mark}] {row['target']} | {row['model']} | {row['status']} | {row['elapsed_ms']}ms | {row['detail']}")

    has_fail = any(not row["ok"] for row in rows)
    return 1 if has_fail else 0


if __name__ == "__main__":
    sys.exit(main())
