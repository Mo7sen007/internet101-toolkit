#!/usr/bin/env python3
"""Tiny TF-IDF search engine for the Internet 101 mini-web.

Documents are read from the SQLite database written by crawler.py. Scoring is
deliberately simple and explainable:

  * text is lowercased and split into alphanumeric words;
  * title words count three times, description/keywords twice, body once;
  * each query term contributes tf * idf, where idf rewards rare terms;
  * results are snippets of the body with the query terms highlighted.

Use it from the web UI (app.py) or straight from a terminal:

    python3 search.py "packet switching"
    python3 search.py --json "dns"
"""

from __future__ import annotations

import argparse
import html
import json
import math
import re
import sqlite3
import sys
from collections import Counter
from pathlib import Path

DEFAULT_DB = Path(__file__).resolve().with_name("index.db")

TOKEN_RE = re.compile(r"[a-z0-9]+")

STOPWORDS = {
    "a", "about", "above", "after", "again", "all", "am", "an", "and", "any",
    "are", "as", "at", "be", "because", "been", "before", "being", "below",
    "between", "both", "but", "by", "can", "could", "did", "do", "does",
    "doing", "down", "during", "each", "few", "for", "from", "further",
    "had", "has", "have", "having", "he", "her", "here", "hers", "herself",
    "him", "himself", "his", "how", "i", "if", "in", "into", "is", "it",
    "its", "itself", "just", "me", "more", "most", "my", "myself", "no",
    "nor", "not", "now", "of", "off", "on", "once", "only", "or", "other",
    "our", "ours", "ourselves", "out", "over", "own", "same", "she",
    "should", "so", "some", "such", "than", "that", "the", "their",
    "theirs", "them", "themselves", "then", "there", "these", "they",
    "this", "those", "through", "to", "too", "under", "until", "up", "very",
    "was", "we", "were", "what", "when", "where", "which", "while", "who",
    "whom", "why", "will", "with", "you", "your", "yours", "yourself",
    "yourselves", "also", "may", "might", "must", "shall", "us", "via",
}


def tokenize(text: str) -> list[str]:
    """Lowercase, split into words and drop stopwords / one-letter tokens."""
    return [t for t in TOKEN_RE.findall((text or "").lower()) if len(t) > 1 and t not in STOPWORDS]


def highlight_terms(text: str, terms: list[str]) -> str:
    """Escape text and wrap whole-word query terms in <mark> tags."""
    escaped = html.escape(text)
    if not terms:
        return escaped
    pattern = re.compile(
        r"(?<![a-z0-9])(" + "|".join(re.escape(t) for t in sorted(set(terms), key=len, reverse=True)) + r")(?![a-z0-9])",
        re.IGNORECASE,
    )
    return pattern.sub(lambda m: f"<mark>{m.group(1)}</mark>", escaped)


def make_snippet(text: str, terms: list[str], width: int = 230) -> str:
    """Return a short, escaped excerpt around the first matching term."""
    clean = re.sub(r"\s+", " ", text or "").strip()
    if not clean:
        return ""

    lowered = clean.lower()
    position = -1
    for term in terms:
        found = lowered.find(term.lower())
        if found != -1 and (position == -1 or found < position):
            position = found

    if position == -1:
        start = 0
    else:
        start = max(0, position - width // 3)
        # Prefer starting on a word boundary.
        space = clean.rfind(" ", start, position)
        if space != -1:
            start = space + 1

    end = min(len(clean), start + width)
    if end < len(clean):
        space = clean.find(" ", end)
        if space != -1 and space - end < 30:
            end = space

    fragment = clean[start:end].strip()
    prefix = "… " if start > 0 else ""
    suffix = " …" if end < len(clean) else ""
    return prefix + highlight_terms(fragment, terms) + suffix


class SearchEngine:
    """Loads the crawl database into memory and answers ranked queries."""

    def __init__(self, db_path: Path | str = DEFAULT_DB) -> None:
        self.db_path = Path(db_path)
        self.documents: list[dict] = []
        self.term_frequencies: list[dict[str, float]] = []
        self.document_frequency: Counter[str] = Counter()
        self._load()

    def _load(self) -> None:
        if not self.db_path.exists():
            return
        try:
            conn = sqlite3.connect(self.db_path, timeout=10)
            conn.row_factory = sqlite3.Row
            try:
                rows = conn.execute(
                    "SELECT url, domain, title, description, keywords, body, fetched_at "
                    "FROM documents ORDER BY fetched_at DESC"
                ).fetchall()
            finally:
                conn.close()
        except sqlite3.Error:
            return

        for row in rows:
            self.documents.append(dict(row))

        for doc in self.documents:
            counts: Counter[str] = Counter()
            for term in tokenize(doc.get("title", "")):
                counts[term] += 3.0
            for term in tokenize(doc.get("description", "")):
                counts[term] += 2.0
            for term in tokenize(doc.get("keywords", "")):
                counts[term] += 2.0
            for term in tokenize(doc.get("body", "")):
                counts[term] += 1.0

            total = sum(counts.values()) or 1.0
            normalized = {term: count / total for term, count in counts.items()}
            self.term_frequencies.append(normalized)
            for term in normalized:
                self.document_frequency[term] += 1

    @property
    def size(self) -> int:
        return len(self.documents)

    def search(self, query: str, limit: int = 20) -> list[dict]:
        terms = tokenize(query)
        if not terms or not self.documents:
            return []

        unique_terms = list(dict.fromkeys(terms))
        total_docs = len(self.documents)
        scores = [0.0] * total_docs

        for term in unique_terms:
            df = self.document_frequency.get(term, 0)
            if df == 0:
                continue
            idf = math.log((total_docs + 1) / (df + 1)) + 1.0
            for index, frequencies in enumerate(self.term_frequencies):
                weight = frequencies.get(term)
                if weight:
                    scores[index] += idf * weight

        # Small bonus when the whole query appears verbatim in the title.
        phrase = query.strip().lower()
        if len(unique_terms) > 1:
            for index, doc in enumerate(self.documents):
                if scores[index] and phrase in (doc.get("title") or "").lower():
                    scores[index] *= 1.35

        ranked = sorted(
            ((score, index) for index, score in enumerate(scores) if score > 0),
            key=lambda pair: (-pair[0], self.documents[pair[1]].get("title", "").lower()),
        )

        results = []
        for score, index in ranked[:limit]:
            doc = self.documents[index]
            snippet_source = doc.get("body") or doc.get("description") or ""
            snippet = make_snippet(snippet_source, unique_terms)
            if not snippet:
                snippet = make_snippet(doc.get("title", ""), unique_terms)
            results.append(
                {
                    "url": doc.get("url", ""),
                    "domain": doc.get("domain", ""),
                    "title": doc.get("title") or doc.get("url", ""),
                    "description": doc.get("description", ""),
                    "snippet": snippet,
                    "score": round(score, 4),
                    "fetched_at": doc.get("fetched_at", ""),
                }
            )
        return results


def index_stats(db_path: Path | str = DEFAULT_DB) -> dict:
    """Return counts and the most recent crawl run, tolerating a missing DB."""
    db_path = Path(db_path)
    stats = {
        "documents": 0,
        "domains": 0,
        "sites": [],
        "last_crawl": None,
        "db_path": str(db_path),
        "exists": db_path.exists(),
    }
    if not db_path.exists():
        return stats

    try:
        conn = sqlite3.connect(db_path, timeout=10)
        conn.row_factory = sqlite3.Row
        try:
            stats["documents"] = conn.execute("SELECT COUNT(*) FROM documents").fetchone()[0]
            stats["domains"] = conn.execute(
                "SELECT COUNT(DISTINCT domain) FROM documents"
            ).fetchone()[0]
            stats["sites"] = [
                dict(row)
                for row in conn.execute(
                    "SELECT domain, COUNT(*) AS pages FROM documents "
                    "GROUP BY domain ORDER BY domain"
                ).fetchall()
            ]
            row = conn.execute(
                "SELECT finished_at, pages, errors FROM crawl_runs ORDER BY id DESC LIMIT 1"
            ).fetchone()
            if row:
                stats["last_crawl"] = dict(row)
        finally:
            conn.close()
    except sqlite3.Error:
        pass
    return stats


def main() -> None:
    parser = argparse.ArgumentParser(description="Search the Internet 101 index")
    parser.add_argument("query", nargs="+", help="search terms")
    parser.add_argument("--db", default=str(DEFAULT_DB))
    parser.add_argument("--limit", type=int, default=10)
    parser.add_argument("--json", action="store_true", help="print machine-readable JSON")
    args = parser.parse_args()

    query = " ".join(args.query)
    engine = SearchEngine(args.db)
    results = engine.search(query, limit=args.limit)

    if args.json:
        print(json.dumps({"query": query, "count": len(results), "results": results}, indent=2))
        return

    if not results:
        print(f'No results for "{query}". Is the index built? Try: python3 crawler.py')
        return

    print(f'{len(results)} result(s) for "{query}"\n')
    for position, result in enumerate(results, start=1):
        snippet = html.unescape(re.sub(r"<[^>]+>", "", result["snippet"]))
        print(f"{position}. {result['title']}")
        print(f"   {result['url']}   [score {result['score']}]")
        if snippet:
            print(f"   {snippet}")
        print()


if __name__ == "__main__":
    sys.exit(main())
