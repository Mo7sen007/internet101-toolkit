#!/usr/bin/env python3
"""Domain Portal for the Internet 101 workshop.

A tiny web app that lets participants claim a name on the workshop DNS server.
It talks to mini-dns-server over its HTTP API, so it never touches DNS packets
directly.

Run it on the Raspberry Pi:

    python3 app.py                 # http://<pi-ip>/        (port 80, needs root)
    python3 app.py --port 8000     # http://<pi-ip>:8000/

The page pre-fills the visitor's IP address, so registering a domain is a
two-click operation during the workshop.
"""

from __future__ import annotations

import argparse
import ipaddress
import json
import os
import re
import socket
import urllib.error
import urllib.parse
import urllib.request

from flask import Flask, jsonify, render_template, request

DEFAULT_DNS_API = os.environ.get("DNS_API", "http://127.0.0.1:8090")
DEFAULT_TLD = os.environ.get("WORKSHOP_TLD", "web").lstrip(".")

app = Flask(__name__)

DNS_API = DEFAULT_DNS_API
TLD = DEFAULT_TLD

DOMAIN_RE = re.compile(
    r"^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?"
    r"(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$"
)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def detect_lan_ip() -> str:
    """Best-effort guess of this machine's LAN address (no packets are sent)."""
    try:
        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as s:
            s.connect(("8.8.8.8", 80))
            return s.getsockname()[0]
    except OSError:
        return "127.0.0.1"


def client_ip() -> str:
    """Return the visitor's address, honouring a single proxy hop."""
    forwarded = request.headers.get("X-Forwarded-For", "")
    if forwarded:
        candidate = forwarded.split(",")[0].strip()
        if candidate:
            return candidate
    addr = request.remote_addr or "127.0.0.1"
    if addr in ("::1", "localhost"):
        # Someone is browsing on the Pi itself; the Pi's LAN address is far more
        # useful as a pre-filled target than loopback.
        return detect_lan_ip()
    if addr.startswith("::ffff:"):
        return addr[len("::ffff:") :]
    return addr


def normalize_domain(raw: str) -> str:
    domain = (raw or "").strip().lower().rstrip(".")
    if domain.startswith(("http://", "https://")):
        domain = domain.split("://", 1)[1]
    domain = domain.split("/", 1)[0]
    domain = domain.split(":", 1)[0]
    if domain and "." not in domain:
        domain = f"{domain}.{TLD}"
    return domain


def dns_api_request(method: str, path: str, payload: dict | None = None):
    """Call the DNS server's HTTP API and return (status, decoded_json)."""
    url = DNS_API.rstrip("/") + path
    data = None
    headers = {"Accept": "application/json"}
    if payload is not None:
        data = json.dumps(payload).encode("utf-8")
        headers["Content-Type"] = "application/json"

    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=4) as resp:
            raw = resp.read()
            return resp.status, json.loads(raw or b"{}")
    except urllib.error.HTTPError as exc:
        raw = exc.read()
        try:
            body = json.loads(raw or b"{}")
        except json.JSONDecodeError:
            body = {"ok": False, "error": raw.decode("utf-8", "replace")}
        return exc.code, body
    except (urllib.error.URLError, OSError, TimeoutError) as exc:
        return 502, {
            "ok": False,
            "error": f"DNS server unreachable at {url} ({exc})",
        }


# ---------------------------------------------------------------------------
# Pages
# ---------------------------------------------------------------------------


@app.get("/")
def index():
    return render_template(
        "index.html",
        default_ip=client_ip(),
        tld=TLD,
        dns_api=DNS_API,
    )


@app.get("/health")
def health():
    status, body = dns_api_request("GET", "/api/health")
    return jsonify({"ok": status == 200 and body.get("ok", False), "dns": body}), (
        200 if status == 200 else 502
    )


# ---------------------------------------------------------------------------
# JSON API used by the page (proxied to mini-dns-server)
# ---------------------------------------------------------------------------


@app.get("/api/records")
def api_records():
    status, body = dns_api_request("GET", "/api/records")
    return jsonify(body), status


@app.post("/api/register")
def api_register():
    data = request.get_json(silent=True) or request.form
    domain = normalize_domain(str(data.get("domain", "")))
    ip = str(data.get("ip", "")).strip()

    if not domain:
        return jsonify({"ok": False, "error": "Please enter a domain name."}), 400
    if not DOMAIN_RE.match(domain):
        return jsonify(
            {
                "ok": False,
                "error": "Use letters, numbers and hyphens, like alice.web or team-7.pi.",
            }
        ), 400
    if not domain.endswith("." + TLD):
        return jsonify(
            {"ok": False, "error": f"Domains must end in .{TLD}."}
        ), 400

    try:
        parsed = ipaddress.ip_address(ip)
    except ValueError:
        return jsonify(
            {"ok": False, "error": "Enter a valid IPv4 address, like 192.168.1.42."}
        ), 400
    if parsed.version != 4:
        return jsonify({"ok": False, "error": "Only IPv4 addresses are supported."}), 400

    status, body = dns_api_request(
        "POST", "/api/register", {"domain": domain, "ip": str(parsed)}
    )
    if status != 200:
        body.setdefault("ok", False)
        body.setdefault("error", "The DNS server rejected the registration.")
    return jsonify(body), status


@app.delete("/api/records/<path:domain>")
def api_delete(domain: str):
    normalized = normalize_domain(domain)
    status, body = dns_api_request(
        "DELETE", f"/api/records?domain={urllib.parse.quote(normalized)}"
    )
    return jsonify(body), status


def main() -> None:
    global DNS_API, TLD

    parser = argparse.ArgumentParser(description="Internet 101 Domain Portal")
    parser.add_argument("--host", default="0.0.0.0", help="interface to bind")
    parser.add_argument("--port", type=int, default=80, help="HTTP port (default 80)")
    parser.add_argument("--dns-api", default=DEFAULT_DNS_API, help="mini-dns-server API URL")
    parser.add_argument("--tld", default=DEFAULT_TLD, help="workshop TLD without the dot")
    args = parser.parse_args()

    DNS_API = args.dns_api.rstrip("/")
    TLD = args.tld.lstrip(".")
    app.config["DNS_API"] = DNS_API
    app.config["WORKSHOP_TLD"] = TLD

    try:
        app.run(host=args.host, port=args.port, threaded=True, debug=False)
    except PermissionError:
        raise SystemExit(
            f"Port {args.port} needs root. Try:\n"
            f"  sudo python3 app.py --port {args.port}\n"
            f"or, for development:\n"
            f"  python3 app.py --port 8000"
        )
    except OSError as exc:
        raise SystemExit(
            f"Could not start the portal on {args.host}:{args.port}: {exc}\n"
            f"Try another port with --port 8000."
        )


if __name__ == "__main__":
    main()
