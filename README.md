# Internet 101 — Workshop Toolkit

A complete, self-contained software toolkit for running **Internet 101**, a
hands-on workshop that rebuilds the World Wide Web on a local network.

A Raspberry Pi 4 acts as the central hub (DNS, domain portal, search engine).
Each participant's laptop runs a **mini web server** and becomes a real website
on the workshop internet. No internet connection is required.

The code is intentionally small, dependency-light and heavily commented, so
participants can read it after the workshop and see that the web is “just”
names, addresses, sockets and text.

The **mini HTTP server** is the exception to the “runs on the Pi” rule: it is
built to run on participant **Windows laptops**, compiles to a single `.exe`,
and includes a simple live web interface (`/__workshop`) so attendees can watch
their own server work.

---

## Table of contents

1. [Architecture](#architecture)
2. [Repository layout](#repository-layout)
3. [Quick start on the Raspberry Pi](#quick-start-on-the-raspberry-pi)
4. [Running the web server on Windows](#running-the-web-server-on-windows)
5. [Configuring workshop clients](#configuring-workshop-clients)
6. [Component guides](#component-guides)
   - [mini-http-server](#1-mini-http-server-go)
   - [mini-dns-server](#2-mini-dns-server-go)
   - [domain-portal](#3-domain-portal-python--flask)
   - [mini-crawler-search](#4-mini-crawler-search-python-3)
7. [Suggested workshop runbook](#suggested-workshop-runbook)
8. [Troubleshooting](#troubleshooting)
9. [Security and limitations](#security-and-limitations)

---

## Architecture

```text
                          Raspberry Pi 4  (the workshop "internet")
        ┌───────────────────────────────────────────────────────────────┐
        │                                                               │
        │  mini-dns-server       UDP :53      .web / .pi A records      │
        │    └ HTTP API          TCP :8090    register / list records   │
        │                                                               │
        │  domain-portal         TCP :80      claim a name              │
        │  mini-crawler-search   TCP :8081    crawl + TF-IDF search     │
        │                                                               │
        └───────────────▲──────────────────────────────▲────────────────┘
                        │ DNS queries                  │ HTTP crawl
                        │ HTTP registration            │
        ┌───────────────┴───────────────┐   ┌──────────┴────────────────┐
        │ Laptop A                      │   │ Laptop B                  │
        │ mini-http-server  TCP :8080   │   │ mini-http-server  :8080   │
        │ static site in ./public       │   │ static site in ./public   │
        └───────────────────────────────┘   └───────────────────────────┘
                        ▲
                        │ browser: http://alice.web:8080
                        │
                  participant browser
```

**The flow participants experience**

1. A browser asks the Pi: “where is `alice.web`?”
2. `mini-dns-server` answers `192.168.1.42` with **TTL 0** (never cached).
3. The browser opens a TCP connection to port **8080** on Alice's laptop.
4. `mini-http-server` answers with the files in `public/`.
5. The crawler on the Pi discovers every registered `.web` site, follows links,
   indexes the text, and the search page ranks results with TF-IDF.

### Ports

| Port | Protocol | Service | Runs on |
| ---- | -------- | ------- | ------- |
| 53 | UDP | Workshop DNS | Raspberry Pi |
| 80 | TCP | Domain portal (fallback: 8000) | Raspberry Pi |
| 8080 | TCP | Mini HTTP server (HTTP or raw TCP) | Each laptop |
| 8090 | TCP | DNS management API | Raspberry Pi |
| 8081 | TCP | Crawler + search UI | Raspberry Pi |

---

## Repository layout

```text
internet101-toolkit/
├── mini-http-server/
│   ├── main.go                  # HTTP + raw TCP server, stdlib only, Windows-friendly
│   └── public/
│       └── index.html           # default workshop page + live status widget
├── mini-dns-server/
│   ├── main.go                  # DNS wire protocol, registry, forwarding, HTTP API
│   └── go.mod
├── domain-portal/
│   ├── app.py                   # Flask portal
│   ├── requirements.txt
│   └── templates/
│       └── index.html           # registration form + live table
├── mini-crawler-search/
│   ├── crawler.py               # recursive crawler -> SQLite
│   ├── search.py                # TF-IDF engine + CLI
│   ├── app.py                   # search UI + crawl trigger API
│   ├── requirements.txt
│   └── templates/
│       └── index.html           # search page
└── README.md
```

---

## Quick start on the Raspberry Pi

These steps assume Raspberry Pi OS (64-bit) or any Debian-based ARM64 system.

### 1. Install Go and Python dependencies

```bash
sudo apt update
sudo apt install -y golang-go python3 python3-pip python3-venv
```

### 2. Build and start the DNS server

```bash
cd mini-dns-server
go build -o mini-dns-server .
sudo ./mini-dns-server          # port 53 needs root or CAP_NET_BIND_SERVICE
```

Before this works, make sure nothing else owns port 53. On Raspberry Pi OS with
`systemd-resolved`, disable the stub listener once:

```bash
sudo sed -i 's/^#\?DNSStubListener=.*/DNSStubListener=no/' /etc/systemd/resolved.conf
sudo systemctl restart systemd-resolved
```

(or stop/disable `dnsmasq` if you installed it). Check with:

```bash
sudo ss -lunp | grep ':53'
```

### 3. Start the domain portal

```bash
cd domain-portal
python3 -m venv .venv && . .venv/bin/activate
pip install -r requirements.txt
sudo .venv/bin/python app.py    # port 80; or: python3 app.py --port 8000
```

### 4. Start the search engine

```bash
cd mini-crawler-search
python3 -m venv .venv && . .venv/bin/activate
pip install -r requirements.txt
python3 app.py                  # http://<pi-ip>:8081/
```

The first crawl can also be triggered from the command line:

```bash
python3 crawler.py              # seeds come from the DNS registry
```

### 5. Start a web server on a laptop

```bash
cd mini-http-server
go build -o mini-http-server main.go
./mini-http-server              # http://<laptop-ip>:8080/
```

Now open the portal from any device, register a name, and browse to
`http://yourname.web:8080`.

> **Optional polish:** register the Pi's own services so the built-in links work:
>
> ```bash
> PI_IP=$(hostname -I | awk '{print $1}')
> curl -s -X POST http://127.0.0.1:8090/api/register \
>      -H 'Content-Type: application/json' \
>      -d "{\"domain\":\"portal.web\",\"ip\":\"$PI_IP\"}"
> curl -s -X POST http://127.0.0.1:8090/api/register \
>      -H 'Content-Type: application/json' \
>      -d "{\"domain\":\"search.web\",\"ip\":\"$PI_IP\"}"
> ```
>
> Then `http://portal.web` and `http://search.web:8081` work for everyone.

### Firewall on the Pi

```bash
sudo ufw allow 53/udp
sudo ufw allow 80/tcp
sudo ufw allow 8081/tcp
sudo ufw allow 8090/tcp
```

`8080/tcp` must be open on **each participant laptop**, not on the Pi.

---

## Running the web server on Windows

`mini-http-server` is the one component explicitly designed to run on
participant laptops, and Windows is its primary target.

### Get the executable

Build it on any machine with Go installed:

```powershell
cd mini-http-server
$env:GOOS = "windows"
$env:GOARCH = "amd64"
go build -o mini-http-server.exe main.go
```

Copy `mini-http-server.exe` **and** the `public` folder into the same directory
on the Windows laptop, for example `C:\Internet101\`.

### Run it

Double-click `mini-http-server.exe`, or from PowerShell / Command Prompt:

```powershell
cd C:\Internet101
.\mini-http-server.exe --open
```

- `--open` launches the default browser at `http://localhost:8080/`.
- Windows Firewall will ask for permission the first time. Choose **Allow** on
  **Private networks**, otherwise other laptops cannot reach you.
- The console prints every request and the laptop's LAN URL, for example:

  ```text
  mini-http-server 1.0.0 (HTTP mode)
  Serving files from: C:\Internet101\public
  Listening on: 0.0.0.0:8080
    -> http://localhost:8080/
    -> http://192.168.1.42:8080/
  Workshop dashboard: http://localhost:8080/__workshop
  ```

### The simple web interface

There are two pages:

| URL | Purpose |
| --- | ------- |
| `http://localhost:8080/` | The normal static site from `public/` |
| `http://localhost:8080/__workshop` | Live dashboard: uptime, request log, local URLs, file list (auto-refreshes) |

`http://localhost:8080/__status` returns the same information as JSON. The
default `public/index.html` also shows a live request counter by calling that
endpoint, so participants immediately see their own server working.

### Edit the site

Open `C:\Internet101\public\index.html` in Notepad or VS Code, save, then
refresh the browser. Add CSS, images or extra pages to the same folder. If the
`public` folder is missing, the server recreates `index.html` on startup.

### Raw TCP demo on Windows

Stop the HTTP server, then run:

```powershell
.\mini-http-server.exe --raw
```

Open `http://192.168.1.42:8080/` in a browser. The browser still sends an HTTP
request, but the raw server prints those bytes to the console and answers with
plain text and **no HTTP headers** — a memorable demonstration that HTTP is
just text inside a TCP connection. `telnet` or a small PowerShell `TcpClient`
script works too.

Other useful flags: `--port 9000`, `--host 0.0.0.0`, `--dir D:\site`, `--version`.

---

## Configuring workshop clients

Workshop names such as `.web` and `.pi` are **not real internet TLDs**. They only
exist because `mini-dns-server` is answering DNS queries.

### Recommended: configure the router

Give the Pi a DHCP reservation / static address (for example
`192.168.1.10`), then set the router's DHCP **DNS server** to that address.
Every laptop that reconnects will use the Pi automatically.

### Manual per-device setup

**Windows**

1. Settings → Network & Internet → Wi-Fi/Ethernet → adapter properties.
2. IPv4 properties → “Use the following DNS server addresses”.
3. Preferred DNS: the Pi's IP (for example `192.168.1.10`).
4. `ipconfig /flushdns` in Command Prompt.

**macOS**

System Settings → Network → Wi-Fi → Details → DNS → add the Pi's IP.

**Linux**

```bash
sudo resolvectl dns <interface> 192.168.1.10
# or edit /etc/resolv.conf:
nameserver 192.168.1.10
```

**Test it**

```bash
dig @192.168.1.10 alice.web A       # Linux/macOS
nslookup alice.web 192.168.1.10     # Windows
```

The answer must show an `A` record and `TTL 0`.

Remember that the workshop TLD and the web server port are separate things:

```text
http://alice.web:8080/     ← DNS gives the IP; :8080 selects the web server
```

---

## Component guides

### 1. mini-http-server (Go)

**Goal:** show what a web server actually does: accept a TCP connection, read a
request, write bytes back, close the connection.

**Standard library only.** One `main.go`, cross-compiles to Linux, macOS and
Windows.

#### HTTP mode (default)

```bash
./mini-http-server --port 8080 --dir public
```

- Serves files from `public/` (resolved relative to the working directory, or
  next to the executable when double-clicked on Windows).
- Explicitly sets `Content-Type`, `Content-Length`, `Connection: close`,
  `Cache-Control: no-store` and `X-Content-Type-Options: nosniff`.
- Logs one clean line per request:

  ```text
  [GET] /index.html - Client: 192.168.1.23:51544 (200, 6883 bytes, 1ms)
  ```

- Path traversal is rejected; directories fall back to their `index.html`.
- Custom 404 page.
- `/__workshop` dashboard and `/__status` JSON endpoint (also implemented with
  the standard library).
- Creates `public/index.html` with a workshop-themed page if none exists.

#### Raw TCP mode

```bash
./mini-http-server --raw --port 8080
```

- Listens on a plain TCP socket (no HTTP parsing at all).
- Prints received bytes to stdout, framed between markers.
- Replies with a short plain-text message, then closes the socket.
- Works with `telnet`, `nc`, a browser, or a PowerShell `TcpClient`.

#### Flags

| Flag | Default | Meaning |
| ---- | ------- | ------- |
| `--raw` | off | Raw TCP mode instead of HTTP |
| `--port` | `8080` | Listening port |
| `--host` | `0.0.0.0` | Bind address |
| `--dir` | `public` | Static file directory |
| `--open` | off | Open the default browser on start (HTTP mode) |
| `--version` | | Print version |

#### Building for other platforms

```bash
# Linux ARM64 (Raspberry Pi 4, 64-bit OS)
GOOS=linux GOARCH=arm64 go build -o mini-http-server main.go

# Linux x86_64
GOOS=linux GOARCH=amd64 go build -o mini-http-server main.go

# Windows x86_64
GOOS=windows GOARCH=amd64 go build -o mini-http-server.exe main.go

# macOS (Apple Silicon)
GOOS=darwin GOARCH=arm64 go build -o mini-http-server main.go
```

Run `go build ... main.go` from inside `mini-http-server/` because the component
is intentionally a single-file program with no module file.

---

### 2. mini-dns-server (Go)

**Goal:** show that DNS is a small, understandable protocol: one question,
one answer, a TTL.

**Zero third-party dependencies.** The DNS wire format is implemented directly
with `encoding/binary`, so attendees can read every byte.

#### Features

- UDP DNS server on port 53 (configurable).
- Authoritative A records for `.web` and `.pi` names from a thread-safe
  in-memory registry.
- **Every local answer uses TTL 0**, so changing a record takes effect
  immediately in browsers and operating systems.
- Unknown workshop names return `NXDOMAIN`; non-A queries for workshop names
  return `NOERROR` with no answers (NODATA).
- Anything outside the workshop TLDs is forwarded to an upstream resolver
  (default `1.1.1.1:53`), using UDP relay with timeouts and `SERVFAIL` on
  failure. Forwarded answers are relayed unchanged, so ordinary internet names
  keep their real TTLs.
- Case-insensitive lookups and DNS compression-pointer support.
- Run as many instances as you like with `--port` for testing without root.

#### Run

```bash
cd mini-dns-server
go build -o mini-dns-server .
sudo ./mini-dns-server
# or, without root, for experiments:
./mini-dns-server --port 5353 --api-port 8090
```

Useful flags:

| Flag | Default | Meaning |
| ---- | ------- | ------- |
| `--port` | `53` | DNS UDP port |
| `--host` | `0.0.0.0` | DNS bind address |
| `--api-port` | `8090` | HTTP management API port |
| `--api-host` | `0.0.0.0` | API bind address |
| `--upstream` | `1.1.1.1:53` | Fallback resolver (`""` disables forwarding) |
| `--tlds` | `.web,.pi` | Workshop TLDs |
| `--default-tld` | `.web` | Appended when a name has no dot |

If port 53 is refused, either run with `sudo`, grant the capability once:

```bash
sudo setcap 'cap_net_bind_service=+ep' ./mini-dns-server
```

or test on a high port with `--port 5353`.

#### HTTP API (`:8090`)

`GET /api/records`

```json
{
  "count": 1,
  "records": [
    {
      "domain": "alice.web",
      "ip": "192.168.1.42",
      "updated": "2026-11-16T17:50:04Z"
    }
  ],
  "tlds": [".web", ".pi"],
  "upstream": "1.1.1.1:53"
}
```

`POST /api/register` — accepts JSON or form encoding:

```bash
curl -X POST http://127.0.0.1:8090/api/register \
     -H 'Content-Type: application/json' \
     -d '{"domain":"alice","ip":"192.168.1.42"}'
```

`alice` is expanded to `alice.web`. Re-registering an existing name updates it
and returns `"created": false`.

`DELETE /api/records?domain=alice.web` removes a record.

`GET /api/health` returns the record count and configured TLDs.

CORS headers are included so browser-based tools can call the API directly.
The registry is intentionally in memory: restarting the DNS server clears it,
which is a useful teaching moment (and easy to recover from via the portal).

---

### 3. domain-portal (Python / Flask)

**Goal:** make DNS registration a two-click exercise, and show a live view of
every name on the workshop internet.

#### Features

- Responsive single-page UI on port 80 (`--port 8000` for unprivileged use).
- Domain field auto-appends `.web` when no dot is typed.
- IP field is pre-filled with the visitor's detected address. If the visitor is
  on the Pi itself, the Pi's LAN address is suggested instead of `::1`.
- Server-side validation with friendly error messages (domain labels, TLD,
  IPv4).
- Registration is proxied to `mini-dns-server` — the browser never needs direct
  access to port 8090.
- Live table of records, refreshed every 3 seconds, with filter, copy, open and
  delete actions. Links go to `http://<domain>:8080/`.
- Clear online/offline indicator for the DNS API.

#### Run

```bash
cd domain-portal
python3 -m venv .venv && . .venv/bin/activate
pip install -r requirements.txt
sudo .venv/bin/python app.py              # port 80
python3 app.py --port 8000                # development
```

Flags:

| Flag | Default | Meaning |
| ---- | ------- | ------- |
| `--host` | `0.0.0.0` | Bind address |
| `--port` | `80` | HTTP port |
| `--dns-api` | `http://127.0.0.1:8090` | DNS API base URL |
| `--tld` | `web` | Workshop TLD (without the dot) |

Environment variables `DNS_API` and `WORKSHOP_TLD` are also supported.

#### Endpoints used by the page

- `GET /` — the portal page.
- `GET /api/records` — proxied record list.
- `POST /api/register` — `{"domain": "...", "ip": "..."}`, proxied registration.
- `DELETE /api/records/<domain>` — proxied deletion.
- `GET /health` — DNS API reachability.

---

### 4. mini-crawler-search (Python 3)

**Goal:** show how a search engine builds an index by following links — and why
a small TF-IDF scorer is enough to rank a mini-web.

#### Crawler (`crawler.py`)

- Seeds from:
  - explicit URLs on the command line, or
  - `GET http://127.0.0.1:8090/api/records` on the DNS server.
- Fetches HTML with `requests`, parses with BeautifulSoup.
- Extracts `<title>`, meta description/keywords, body text and `<a href>` links.
- Resolves relative links, follows redirects, skips non-HTML and oversized
  pages, and reports HTTP errors without aborting the crawl.
- Loop detection via a per-crawl `visited` set, plus depth and page limits.
- Only crawls workshop TLDs (`.web`, `.pi`) on port 8080 by default, so it does
  not wander into the portal, the search UI, or the public internet.
- Stores documents in SQLite (`index.db`) with upsert semantics, and records
  each crawl run.
- Polite delay between requests (150 ms by default) and a clear User-Agent.

```bash
python3 crawler.py                              # all registered .web sites
python3 crawler.py alice.web                    # bare domain, port 8080 assumed
python3 crawler.py http://alice.web:8080/       # one seed
python3 crawler.py --max-pages 50 --depth 1 --delay 0.3
python3 crawler.py --skip-indexed               # only fetch new URLs
python3 crawler.py --ports 8080,8000            # allow other web server ports
python3 crawler.py --verbose
```

Database schema:

```sql
documents(url PRIMARY KEY, domain, title, description, keywords, body, fetched_at)
crawl_runs(id, started_at, finished_at, pages, errors, seeds)
```

#### Search engine (`search.py`)

Scoring is deliberately simple and explainable:

1. Lowercase, split into alphanumeric words, drop stopwords.
2. Weight title terms ×3, description/keywords ×2, body ×1.
3. For each query term compute `tf × idf`, where
   `idf = log((N + 1) / (df + 1)) + 1`.
4. Add a small bonus when the full query appears verbatim in a title.
5. Return the top results with a body snippet and `<mark>` highlights.

Use it from the terminal:

```bash
python3 search.py "packet switching"
python3 search.py --json --limit 5 dns
```

Or from Python:

```python
from search import SearchEngine
engine = SearchEngine("index.db")
for hit in engine.search("http"):
    print(hit["score"], hit["title"], hit["url"])
```

#### Search UI (`app.py`)

- Search page on port 8081 with server-rendered results.
- Shows indexed page/site counts and the last crawl time.
- **Trigger crawl** button starts a background crawl (one at a time) and polls
  `/api/status` until it finishes.
- Index is cached in memory and rebuilt automatically when `index.db` changes.

```bash
python3 app.py                  # http://<pi-ip>:8081/
python3 app.py --port 9000 --max-pages 100 --depth 1
```

API:

- `GET /api/search?q=dns&limit=20`
- `GET /api/status` — crawl state, last run, index statistics
- `POST /api/crawl` — start a crawl; optional body
  `{"seeds": ["http://alice.web:8080/"]}`
- `GET /health`

Result links point directly at the participant's server, for example
`http://alice.web:8080/`.

---

## Suggested workshop runbook

**Before participants arrive**

1. Pi has a fixed LAN address and is reachable over SSH.
2. DNS server, portal and search UI are running.
3. `portal.web` / `search.web` are registered (optional but nice).
4. One laptop is running `mini-http-server.exe --open` as a demo.
5. Clients can resolve a test name: `nslookup test.web <pi-ip>`.

**Workshop flow (≈90 minutes)**

| Time | Activity |
| ---- | -------- |
| 10 min | What is an IP address? What is a port? Ping the Pi. |
| 15 min | Run the mini HTTP server on each laptop; open it from a neighbour's laptop by IP. |
| 20 min | DNS: register names in the portal; watch the live table; browse `http://name.web:8080`. |
| 10 min | Raw TCP: restart the server with `--raw`; connect from a browser or `telnet`; see the bytes. |
| 15 min | Read `main.go` together: request line, headers, body, `Connection: close`. |
| 15 min | Trigger a crawl; search for words from participants' pages; discuss TF-IDF and links. |
| 5 min  | Break things: change a record's IP, re-register, delete a record, refresh. |

**Discussion prompts**

- Why does the DNS answer use TTL 0? What would TTL 300 do to the demo?
- What happens if two laptops claim the same name? (Last registration wins.)
- How does the crawler avoid infinite loops? (Visited set, depth limit.)
- Why is `Connection: close` used here instead of keep-alive?
- What is missing from this mini-web compared to the real web? (TLS, cookies,
  caches, search ranking signals, scale, governance…)

---

## Troubleshooting

| Symptom | Cause / fix |
| ------- | ----------- |
| `bind: address already in use` on port 53 | `systemd-resolved`, `dnsmasq`, or another DNS server is running. Disable the stub listener or use `--port 5353` for testing. |
| `permission denied` binding port 53 or 80 | Run with `sudo`, use `setcap`, or choose a high port (`--port 5353`, `--port 8000`). |
| `alice.web` does not resolve | Client is not using the Pi as its DNS server, or the record was never registered. Test with `dig @<pi-ip> alice.web`. |
| DNS works on the Pi but not elsewhere | Firewall blocks UDP 53, or the Pi and laptops are on different networks / guest Wi-Fi isolation. |
| Browser says “site can't be reached” | The laptop's web server isn't running, port 8080 is blocked by its firewall, or the registered IP is wrong. |
| Browser upgrades to HTTPS | Type `http://` explicitly. Some browsers try HTTPS first for unknown names. |
| Windows laptop invisible to others | Firewall prompt was dismissed. Allow the app on **Private** networks; check the network profile isn't “Public”. |
| Portal says “DNS server unreachable” | `mini-dns-server` isn't running, or the portal was started with the wrong `--dns-api`. |
| Crawl finds zero pages | No domains registered, web servers not running, or servers listen on a port other than 8080 (use `--ports`). |
| Search returns nothing | Index is empty: press **Trigger crawl**, or run `python3 crawler.py`. |
| Stale DNS answer | Should not happen with TTL 0, but flush caches: `ipconfig /flushdns` (Windows), `sudo resolvectl flush-caches` (Linux), restart the browser. |
| Old record still in the table | The table polls every 3 s; press **Refresh**. The registry is in memory and disappears when the DNS server restarts. |

When in doubt, run each component in its own terminal and read its logs. Every
server logs the interesting events in a single human-readable line.

---

## Security and limitations

This toolkit is built for a **trusted, isolated workshop LAN**. It is not
hardened for the public internet.

- No authentication or encryption: DNS is plain UDP, websites are plain HTTP,
  and the registration API accepts requests from anyone who can reach it.
- No persistence: DNS records live in memory and vanish on restart. The crawl
  index persists in `index.db`.
- One A record per domain; IPv4 only. No AAAA, CNAME, MX or wildcard records.
- The DNS server forwards non-workshop names, which makes it an open resolver
  while it runs. Do not expose port 53 to the internet.
- The crawler can be pointed at external hosts with `--allow-external`; leave
  that switch off during the workshop.
- The HTTP server is a teaching tool: static files only, no CGI, no TLS, no
  range requests. That is deliberate.
- Run the portal and DNS API on a network where participants are trusted; the
  API can create and delete any record.

---

Built for curious beginners. Read the code, break it, and rebuild it.
