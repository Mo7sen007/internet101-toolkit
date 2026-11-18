#!/usr/bin/env python3
"""Web crawler for the Internet 101 mini-web.

The crawler starts from the domains registered on the workshop DNS server (or
from URLs you pass on the command line), fetches every reachable HTML page on
the workshop TLDs, extracts titles, meta tags and body text, and stores the
result in a SQLite database (index.db) used by the search engine.

Usage:
    python3 crawler.py                        # seed from mini-dns-server
    python3 crawler.py http://alice.web:8080  # explicit seed
    python3 crawler.py --max-pages 50 --depth 1

Only pages on the workshop TLDs (.web, .pi) and, by default, only port 8080 are
crawled. That keeps the crawler inside the workshop and stops it from wandering
into the portal or the search UI.
"""

from __future__ import annotations

import argparse
import logging
import os
import re
import sqlite3
import time
import urllib.parse
from collections import deque
from datetime import datetime, timezone
from pathlib import Path
from typing import Iterable

import requests
from bs4 import BeautifulSoup

DEFAULT_DNS_API = os.environ.get("DNS_API", "http://127.0.0.1:8090")
DEFAULT_DB = Path(__file__).resolve().with_name("index.db")
DEFAULT_TLDS = (".web", ".pi")
DEFAULT_PORTS = (8080,)
USER_AGENT = "Internet101Crawler/1.0 (workshop; +https://example.invalid)"

log = logging.getLogger("crawler")

SCHEMA = """
CREATE TABLE IF NOT EXISTS documents (
    url         TEXT PRIMARY KEY,
    domain      TEXT NOT NULL,
    title       TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    keywords    TEXT NOT NULL DEFAULT '',
    body        TEXT NOT NULL DEFAULT '',
    fetched_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_documents_domain ON documents(domain);

CREATE TABLE IF NOT EXISTS crawl_runs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at  TEXT NOT NULL,
    finished_at TEXT NOT NULL,
    pages       INTEGER NOT NULL,
    errors      INTEGER NOT NULL,
    seeds       TEXT NOT NULL
);
"""


def utcnow() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def tokenize_host(host: str) -> str:
    return host.lower().rstrip(".")


class Crawler:
    """A polite, depth-limited crawler for the workshop network."""

    def __init__(
        self,
        db_path: Path | str = DEFAULT_DB,
        dns_api: str = DEFAULT_DNS_API,
        tlds: Iterable[str] = DEFAULT_TLDS,
        ports: Iterable[int] = DEFAULT_PORTS,
        timeout: float = 6.0,
        delay: float = 0.15,
        max_pages: int = 200,
        max_depth: int = 2,
        max_bytes: int = 2_000_000,
        allow_external: bool = False,
    ) -> None:
        self.db_path = Path(db_path)
        self.dns_api = dns_api.rstrip("/")
        self.tlds = tuple(t.lower() if t.startswith(".") else "." + t.lower() for t in tlds)
        self.ports = set(int(p) for p in ports)
        self.timeout = timeout
        self.delay = max(0.0, delay)
        self.max_pages = max(1, max_pages)
        self.max_depth = max(0, max_depth)
        self.max_bytes = max_bytes
        self.allow_external = allow_external
        self.session = requests.Session()
        self.session.headers.update(
            {"User-Agent": USER_AGENT, "Accept": "text/html,application/xhtml+xml"}
        )

    # -- database ----------------------------------------------------------

    def connect(self) -> sqlite3.Connection:
        conn = sqlite3.connect(self.db_path, timeout=10)
        conn.row_factory = sqlite3.Row
        return conn

    def init_db(self) -> None:
        with self.connect() as conn:
            conn.executescript(SCHEMA)

    def save_document(self, doc: dict) -> None:
        with self.connect() as conn:
            conn.execute(
                """
                INSERT INTO documents (url, domain, title, description, keywords, body, fetched_at)
                VALUES (:url, :domain, :title, :description, :keywords, :body, :fetched_at)
                ON CONFLICT(url) DO UPDATE SET
                    domain = excluded.domain,
                    title = excluded.title,
                    description = excluded.description,
                    keywords = excluded.keywords,
                    body = excluded.body,
                    fetched_at = excluded.fetched_at
                """,
                doc,
            )

    def record_run(self, started_at: str, finished_at: str, pages: int, errors: int, seeds: list[str]) -> None:
        with self.connect() as conn:
            conn.execute(
                "INSERT INTO crawl_runs (started_at, finished_at, pages, errors, seeds) VALUES (?, ?, ?, ?, ?)",
                (started_at, finished_at, pages, errors, ",".join(seeds[:50])),
            )

    def indexed_urls(self) -> set[str]:
        try:
            with self.connect() as conn:
                return {row["url"] for row in conn.execute("SELECT url FROM documents")}
        except sqlite3.Error:
            return set()

    # -- URL handling ------------------------------------------------------

    def normalize_url(self, url: str, base: str | None = None) -> str | None:
        """Resolve, lowercase and clean a URL. Returns None if unusable."""
        url = (url or "").strip()
        if not url or url.startswith(("#", "mailto:", "tel:", "javascript:", "data:")):
            return None
        if base:
            url = urllib.parse.urljoin(base, url)
        try:
            parts = urllib.parse.urlsplit(url)
            if parts.scheme.lower() not in ("http", "https"):
                return None
            host = tokenize_host(parts.hostname or "")
            if not host:
                return None
            port = parts.port or (443 if parts.scheme.lower() == "https" else 80)
        except ValueError:
            return None

        path = parts.path or "/"
        netloc = host if port in (80, 443) else f"{host}:{port}"
        return urllib.parse.urlunsplit((parts.scheme.lower(), netloc, path, parts.query, ""))

    def is_allowed(self, url: str) -> bool:
        try:
            parts = urllib.parse.urlsplit(url)
            host = tokenize_host(parts.hostname or "")
            port = parts.port or (443 if parts.scheme == "https" else 80)
        except ValueError:
            return False
        if not host:
            return False
        if port not in self.ports:
            return False
        if self.allow_external:
            return True
        return self.is_workshop_host(host)

    def is_workshop_host(self, host: str) -> bool:
        host = tokenize_host(host)
        return any(host == tld.lstrip(".") or host.endswith(tld) for tld in self.tlds)

    def normalize_seed(self, seed: str) -> str | None:
        """Accept both full URLs and bare workshop domains.

        ``alice.web`` becomes ``http://alice.web:8080/`` so that participants can
        pass what they see in the portal without remembering the port.
        """
        raw = (seed or "").strip()
        if not raw:
            return None
        if "://" not in raw:
            raw = "http://" + raw
        try:
            parts = urllib.parse.urlsplit(raw)
            host = tokenize_host(parts.hostname or "")
            if (
                parts.scheme.lower() == "http"
                and host
                and parts.port is None
                and self.is_workshop_host(host)
            ):
                raw = urllib.parse.urlunsplit(
                    ("http", f"{host}:{sorted(self.ports)[0]}", parts.path or "/", parts.query, "")
                )
        except ValueError:
            return None
        return self.normalize_url(raw)

    def discover_seeds(self) -> list[str]:
        """Ask mini-dns-server for every registered workshop domain."""
        url = f"{self.dns_api}/api/records"
        try:
            resp = self.session.get(url, timeout=self.timeout)
            resp.raise_for_status()
            payload = resp.json()
        except (requests.RequestException, ValueError) as exc:
            raise RuntimeError(f"could not fetch seeds from {url}: {exc}") from exc

        seeds: list[str] = []
        for record in payload.get("records", []):
            domain = str(record.get("domain", "")).strip()
            if not domain:
                continue
            seeds.append(f"http://{domain}:{sorted(self.ports)[0]}/")
        log.info("discovered %d seed(s) from the DNS registry", len(seeds))
        return seeds

    # -- extraction --------------------------------------------------------

    @staticmethod
    def extract_page(html: str, page_url: str) -> dict:
        soup = BeautifulSoup(html, "html.parser")
        for tag in soup(["script", "style", "noscript", "template", "svg"]):
            tag.decompose()

        title = ""
        if soup.title and soup.title.string:
            title = soup.title.get_text(" ", strip=True)
        if not title:
            heading = soup.find(["h1", "h2"])
            title = heading.get_text(" ", strip=True) if heading else page_url

        def meta(*names: str) -> str:
            for name in names:
                tag = soup.find("meta", attrs={"name": re.compile(f"^{re.escape(name)}$", re.I)})
                if tag and tag.get("content"):
                    return tag["content"].strip()
            return ""

        description = meta("description", "og:description") or meta("twitter:description")
        keywords = meta("keywords")
        if not description:
            paragraph = soup.find("p")
            if paragraph:
                description = paragraph.get_text(" ", strip=True)

        body = re.sub(r"\s+", " ", soup.get_text(" ", strip=True)).strip()

        links: list[str] = []
        for anchor in soup.find_all("a", href=True):
            links.append(anchor["href"])
            if len(links) >= 500:
                break

        return {
            "title": title[:300],
            "description": description[:500],
            "keywords": keywords[:300],
            "body": body[:60_000],
            "links": links,
        }

    # -- crawling ----------------------------------------------------------

    def crawl(self, seeds: list[str] | None = None, skip_indexed: bool = False) -> dict:
        self.init_db()
        started_at = utcnow()
        started_clock = time.time()

        seed_urls: list[str] = []
        for seed in seeds or []:
            normalized = self.normalize_seed(seed)
            if normalized:
                seed_urls.append(normalized)
        if not seed_urls:
            seed_urls = self.discover_seeds()

        # De-duplicate while preserving order.
        seed_urls = list(dict.fromkeys(seed_urls))

        already_indexed = self.indexed_urls() if skip_indexed else set()
        queue: deque[tuple[str, int]] = deque((url, 0) for url in seed_urls)
        visited: set[str] = set()
        pages = 0
        errors = 0
        skipped = 0

        log.info(
            "crawl started: %d seed(s), max_pages=%d, depth=%d",
            len(seed_urls), self.max_pages, self.max_depth,
        )

        while queue and pages < self.max_pages:
            url, depth = queue.popleft()
            if url in visited:
                continue
            visited.add(url)

            if url in already_indexed:
                skipped += 1
                continue
            if not self.is_allowed(url):
                skipped += 1
                continue

            try:
                resp = self.session.get(url, timeout=self.timeout, allow_redirects=True)
                if resp.status_code >= 400:
                    raise requests.HTTPError(f"HTTP {resp.status_code}")
                content_type = resp.headers.get("Content-Type", "").lower()
                if content_type and "html" not in content_type:
                    skipped += 1
                    log.debug("skip non-HTML %s (%s)", url, content_type)
                    continue
                if len(resp.content) > self.max_bytes:
                    skipped += 1
                    log.debug("skip oversized page %s", url)
                    continue
            except requests.RequestException as exc:
                errors += 1
                log.warning("fetch failed: %s (%s)", url, exc)
                continue

            page = self.extract_page(resp.text, resp.url or url)
            domain = tokenize_host(urllib.parse.urlsplit(resp.url or url).hostname or "")
            self.save_document(
                {
                    "url": url,
                    "domain": domain,
                    "title": page["title"] or domain,
                    "description": page["description"],
                    "keywords": page["keywords"],
                    "body": page["body"],
                    "fetched_at": utcnow(),
                }
            )
            pages += 1
            log.info("[%d/%d] indexed %s (%d chars)", pages, self.max_pages, url, len(page["body"]))

            if depth < self.max_depth:
                for href in page["links"]:
                    link = self.normalize_url(href, base=resp.url or url)
                    if not link or link in visited or link in already_indexed:
                        continue
                    if self.is_allowed(link):
                        queue.append((link, depth + 1))

            if self.delay:
                time.sleep(self.delay)

        finished_at = utcnow()
        duration = time.time() - started_clock
        self.record_run(started_at, finished_at, pages, errors, seed_urls)

        stats = {
            "pages": pages,
            "errors": errors,
            "skipped": skipped,
            "visited": len(visited),
            "queued": len(queue),
            "duration_seconds": round(duration, 2),
            "started_at": started_at,
            "finished_at": finished_at,
            "seeds": seed_urls,
        }
        log.info(
            "crawl finished: %d page(s), %d error(s), %d skipped, %.1fs",
            pages, errors, skipped, duration,
        )
        return stats


def main() -> None:
    parser = argparse.ArgumentParser(description="Crawl the Internet 101 mini-web")
    parser.add_argument("seeds", nargs="*", help="seed URLs or workshop domains (default: ask the DNS server)")
    parser.add_argument("--db", default=str(DEFAULT_DB), help="SQLite database path")
    parser.add_argument("--dns-api", default=DEFAULT_DNS_API, help="mini-dns-server API URL")
    parser.add_argument("--max-pages", type=int, default=200)
    parser.add_argument("--depth", type=int, default=2, help="maximum link depth")
    parser.add_argument("--delay", type=float, default=0.15, help="delay between requests (seconds)")
    parser.add_argument("--timeout", type=float, default=6.0)
    parser.add_argument("--tlds", default=".web,.pi", help="comma separated workshop TLDs")
    parser.add_argument("--ports", default="8080", help="comma separated allowed ports")
    parser.add_argument("--allow-external", action="store_true",
                        help="also crawl hosts outside the workshop TLDs")
    parser.add_argument("--skip-indexed", action="store_true",
                        help="do not re-fetch URLs that are already in the database")
    parser.add_argument("--verbose", "-v", action="store_true")
    args = parser.parse_args()

    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)-7s %(message)s",
        datefmt="%H:%M:%S",
    )

    crawler = Crawler(
        db_path=args.db,
        dns_api=args.dns_api,
        tlds=[t for t in args.tlds.split(",") if t.strip()],
        ports=[int(p) for p in args.ports.split(",") if p.strip()],
        timeout=args.timeout,
        delay=args.delay,
        max_pages=args.max_pages,
        max_depth=args.depth,
        allow_external=args.allow_external,
    )

    try:
        stats = crawler.crawl(seeds=args.seeds or None, skip_indexed=args.skip_indexed)
    except RuntimeError as exc:
        raise SystemExit(f"crawl failed: {exc}")

    print(
        f"\nDone: {stats['pages']} page(s) indexed, {stats['errors']} error(s), "
        f"{stats['duration_seconds']}s. Database: {crawler.db_path}"
    )


if __name__ == "__main__":
    main()
