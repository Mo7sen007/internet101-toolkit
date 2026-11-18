#!/usr/bin/env python3
"""Search UI for the Internet 101 mini-web.

Serves a small search page on port 8081, queries the TF-IDF index built by
crawler.py, and exposes a "Trigger crawl" button for workshop administrators.

Run it on the Raspberry Pi:

    python3 app.py                 # http://<pi-ip>:8081/
    python3 app.py --port 9000

API:
    GET  /api/search?q=dns&limit=20
    GET  /api/status
    POST /api/crawl                (optional JSON body: {"seeds": ["http://..."]})
"""

from __future__ import annotations

import argparse
import logging
import threading
import time
from datetime import datetime, timezone
from pathlib import Path

from flask import Flask, jsonify, render_template, request

from crawler import Crawler
from search import SearchEngine, index_stats

BASE_DIR = Path(__file__).resolve().parent
DEFAULT_DB = BASE_DIR / "index.db"
DEFAULT_DNS_API = "http://127.0.0.1:8090"

app = Flask(__name__)
log = logging.getLogger("search-app")

CONFIG: dict = {
    "db": str(DEFAULT_DB),
    "dns_api": DEFAULT_DNS_API,
    "tlds": (".web", ".pi"),
    "ports": (8080,),
    "max_pages": 200,
    "depth": 2,
    "delay": 0.15,
    "timeout": 6.0,
    "allow_external": False,
}

# ---------------------------------------------------------------------------
# Crawl state (one crawl at a time)
# ---------------------------------------------------------------------------

_crawl_lock = threading.Lock()
_crawl_state: dict = {
    "running": False,
    "started_at": None,
    "started_clock": None,
    "last": None,
    "error": None,
}

_engine_lock = threading.Lock()
_engine_cache: dict = {"key": None, "engine": None}


def get_engine() -> SearchEngine:
    """Rebuild the in-memory index when index.db changes on disk."""
    db = Path(CONFIG["db"])
    try:
        mtime = db.stat().st_mtime_ns
    except OSError:
        mtime = 0
    key = (str(db), mtime)
    with _engine_lock:
        if _engine_cache["key"] != key:
            _engine_cache["engine"] = SearchEngine(db)
            _engine_cache["key"] = key
        return _engine_cache["engine"]


def _run_crawl(seeds: list[str] | None) -> None:
    started = time.time()
    try:
        crawler = Crawler(
            db_path=CONFIG["db"],
            dns_api=CONFIG["dns_api"],
            tlds=CONFIG["tlds"],
            ports=CONFIG["ports"],
            timeout=CONFIG["timeout"],
            delay=CONFIG["delay"],
            max_pages=CONFIG["max_pages"],
            max_depth=CONFIG["depth"],
            allow_external=CONFIG["allow_external"],
        )
        stats = crawler.crawl(seeds=seeds or None)
        with _crawl_lock:
            _crawl_state["last"] = stats
            _crawl_state["error"] = None
    except Exception as exc:  # noqa: BLE001 - surface any failure to the UI
        log.exception("crawl failed")
        with _crawl_lock:
            _crawl_state["error"] = str(exc)
    finally:
        with _crawl_lock:
            _crawl_state["running"] = False
            _crawl_state["elapsed"] = round(time.time() - started, 1)


# ---------------------------------------------------------------------------
# Pages
# ---------------------------------------------------------------------------


@app.get("/")
def index():
    query = (request.args.get("q") or "").strip()[:200]
    results = get_engine().search(query, limit=20) if query else []
    stats = index_stats(CONFIG["db"])
    return render_template(
        "index.html",
        q=query,
        results=results,
        stats=stats,
        dns_api=CONFIG["dns_api"],
        max_pages=CONFIG["max_pages"],
        depth=CONFIG["depth"],
    )


@app.get("/health")
def health():
    stats = index_stats(CONFIG["db"])
    return jsonify({"ok": True, "documents": stats["documents"], "db": stats["db_path"]})


# ---------------------------------------------------------------------------
# JSON API
# ---------------------------------------------------------------------------


@app.get("/api/search")
def api_search():
    query = (request.args.get("q") or "").strip()[:200]
    try:
        limit = max(1, min(50, int(request.args.get("limit", 20))))
    except ValueError:
        limit = 20
    results = get_engine().search(query, limit=limit)
    return jsonify({"query": query, "count": len(results), "results": results})


@app.get("/api/status")
def api_status():
    with _crawl_lock:
        running = _crawl_state["running"]
        started_at = _crawl_state["started_at"]
        started_clock = _crawl_state["started_clock"]
        last = _crawl_state["last"]
        error = _crawl_state["error"]
        elapsed = _crawl_state.get("elapsed")

    elapsed_now = round(time.time() - started_clock, 1) if running and started_clock else elapsed
    return jsonify(
        {
            "crawling": running,
            "started_at": started_at,
            "elapsed_seconds": elapsed_now,
            "last_crawl": last,
            "error": error,
            "stats": index_stats(CONFIG["db"]),
        }
    )


@app.post("/api/crawl")
def api_crawl():
    payload = request.get_json(silent=True) or {}
    raw_seeds = payload.get("seeds") or request.form.getlist("seeds")
    if isinstance(raw_seeds, str):
        raw_seeds = [raw_seeds]
    seeds = [str(s).strip() for s in raw_seeds if str(s).strip()][:20]

    with _crawl_lock:
        if _crawl_state["running"]:
            return jsonify({"ok": False, "error": "a crawl is already running"}), 409
        _crawl_state.update(
            {
                "running": True,
                "started_at": datetime.now(timezone.utc).isoformat(timespec="seconds"),
                "started_clock": time.time(),
                "error": None,
            }
        )

    thread = threading.Thread(target=_run_crawl, args=(seeds or None,), daemon=True)
    thread.start()
    log.info("crawl triggered (seeds=%s)", seeds or "from DNS registry")
    return jsonify({"ok": True, "started": True, "seeds": seeds}), 202


# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------


def main() -> None:
    parser = argparse.ArgumentParser(description="Internet 101 mini search engine")
    parser.add_argument("--host", default="0.0.0.0")
    parser.add_argument("--port", type=int, default=8081)
    parser.add_argument("--db", default=str(DEFAULT_DB))
    parser.add_argument("--dns-api", default=DEFAULT_DNS_API)
    parser.add_argument("--tlds", default=".web,.pi")
    parser.add_argument("--ports", default="8080")
    parser.add_argument("--max-pages", type=int, default=200)
    parser.add_argument("--depth", type=int, default=2)
    parser.add_argument("--delay", type=float, default=0.15)
    parser.add_argument("--timeout", type=float, default=6.0)
    parser.add_argument("--allow-external", action="store_true",
                        help="also crawl hosts outside the workshop TLDs")
    args = parser.parse_args()

    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)-7s %(name)s: %(message)s",
        datefmt="%H:%M:%S",
    )

    CONFIG.update(
        {
            "db": args.db,
            "dns_api": args.dns_api.rstrip("/"),
            "tlds": tuple(t if t.startswith(".") else "." + t for t in args.tlds.split(",") if t.strip()),
            "ports": tuple(int(p) for p in args.ports.split(",") if p.strip()),
            "max_pages": args.max_pages,
            "depth": args.depth,
            "delay": args.delay,
            "timeout": args.timeout,
            "allow_external": args.allow_external,
        }
    )

    log.info("search UI on http://%s:%d (database: %s)", args.host, args.port, args.db)
    app.run(host=args.host, port=args.port, threaded=True, debug=False)


if __name__ == "__main__":
    main()
