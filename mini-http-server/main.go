// Command mini-http-server is the "web server" station of the Internet 101
// workshop.
//
// It has two modes:
//
//	HTTP mode (default) - serves the files in ./public over HTTP on port 8080.
//	Raw TCP mode (--raw) - accepts a raw TCP connection, prints the bytes it
//	                      receives, and replies with a plain string (no HTTP
//	                      headers at all), then closes the socket.
//
// The server is deliberately built with the Go standard library only so it can
// be cross-compiled into a single, dependency-free executable for Linux, macOS
// and Windows. It is designed to be double-click friendly on Windows: it creates
// a default public/index.html if one is missing, prints the LAN URL to open,
// and can launch the default browser with --open.
package main

import (
	"context"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const version = "1.0.0"

// defaultIndexHTML is written to <public>/index.html when no index page exists.
// The repository also ships a richer version of this page, so this constant is
// mainly a safety net for people who copy only the binary onto a laptop.
const defaultIndexHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Internet 101 - My Web Server</title>
  <style>
    :root { color-scheme: dark; }
    body {
      margin: 0; min-height: 100vh; display: grid; place-items: center;
      background: radial-gradient(circle at 20% 10%, #123 0%, #0a0f1c 55%, #060910 100%);
      color: #e6edf7; font: 17px/1.65 system-ui, -apple-system, Segoe UI, sans-serif;
      padding: 2rem;
    }
    main { max-width: 680px; }
    .badge {
      display: inline-block; padding: .25rem .7rem; border-radius: 999px;
      background: #1d4ed822; border: 1px solid #3b82f666; color: #93c5fd;
      font-size: .8rem; letter-spacing: .12em; text-transform: uppercase;
    }
    h1 { font-size: clamp(2rem, 6vw, 3rem); line-height: 1.1; margin: 1rem 0; }
    h1 span { color: #38bdf8; }
    p { color: #a9b7cc; }
    .card {
      margin-top: 2rem; padding: 1.25rem 1.5rem; border-radius: 14px;
      background: #ffffff08; border: 1px solid #ffffff14;
    }
    code { color: #fbbf24; }
    a { color: #7dd3fc; }
  </style>
</head>
<body>
  <main>
    <span class="badge">Internet 101</span>
    <h1>This page is served by <span>your own laptop</span>.</h1>
    <p>
      You are looking at a real HTTP response from the mini web server running on
      this machine. Somewhere on the network, a browser asked for
      <code>/</code> and this file came back over TCP.
    </p>
    <div class="card">
      <strong>Try this:</strong>
      <ul>
        <li>Edit <code>public/index.html</code> and refresh this page.</li>
        <li>Open the workshop dashboard at <a href="/__workshop">/__workshop</a>.</li>
        <li>Watch the server terminal - every request is logged there.</li>
      </ul>
    </div>
  </main>
</body>
</html>
`

// ---------------------------------------------------------------------------
// Request log (kept in memory for the /__workshop dashboard)
// ---------------------------------------------------------------------------

type requestEntry struct {
	Time     string
	Method   string
	Path     string
	Client   string
	Status   int
	Bytes    int
	Duration time.Duration
}

type logStore struct {
	mu      sync.Mutex
	entries []requestEntry
	total   int
}

func (l *logStore) add(e requestEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total++
	l.entries = append(l.entries, e)
	if len(l.entries) > 100 {
		l.entries = l.entries[len(l.entries)-100:]
	}
}

func (l *logStore) snapshot() ([]requestEntry, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]requestEntry, len(l.entries))
	for i, e := range l.entries {
		out[len(l.entries)-1-i] = e // newest first
	}
	return out, l.total
}

// statusWriter records the status code and byte count for logging.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// ---------------------------------------------------------------------------
// HTTP server
// ---------------------------------------------------------------------------

type webServer struct {
	root    string
	port    int
	started time.Time
	logs    *logStore
	tmpl    *template.Template
}

func newWebServer(root string, port int) *webServer {
	s := &webServer{
		root:    root,
		port:    port,
		started: time.Now(),
		logs:    &logStore{},
	}
	s.tmpl = template.Must(template.New("dashboard").Parse(dashboardHTML))
	return s
}

func (s *webServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/__workshop", s.handleDashboard)
	mux.HandleFunc("/__workshop/", s.handleDashboard)
	mux.HandleFunc("/__status", s.handleStatus)
	mux.HandleFunc("/", s.handleStatic)
	return loggingMiddleware(mux, s.logs)
}

// loggingMiddleware prints one clean line per request:
//
//	[GET] /index.html - Client: 192.168.1.23:51544
func loggingMiddleware(next http.Handler, logs *logStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		log.Printf("[%s] %s - Client: %s (%d, %d bytes, %s)",
			r.Method, r.URL.Path, r.RemoteAddr, rec.status, rec.bytes,
			time.Since(start).Round(time.Millisecond))

		logs.add(requestEntry{
			Time:     start.Format("15:04:05"),
			Method:   r.Method,
			Path:     r.URL.RequestURI(),
			Client:   r.RemoteAddr,
			Status:   rec.status,
			Bytes:    rec.bytes,
			Duration: time.Since(start).Round(time.Microsecond),
		})
	})
}

func (s *webServer) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	urlPath := r.URL.Path
	if !strings.HasPrefix(urlPath, "/") {
		urlPath = "/" + urlPath
	}
	clean := path.Clean(urlPath)
	if clean == "/" {
		clean = "/index.html"
	}

	filePath := filepath.Join(s.root, filepath.FromSlash(clean))

	// Reject anything that escapes the public root.
	rel, err := filepath.Rel(s.root, filePath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	info, err := os.Stat(filePath)
	if err == nil && info.IsDir() {
		filePath = filepath.Join(filePath, "index.html")
		info, err = os.Stat(filePath)
	}
	if err != nil || info.IsDir() {
		s.notFound(w, r)
		return
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		log.Printf("error reading %s: %v", filePath, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(filePath)))
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	if strings.HasPrefix(contentType, "text/") && !strings.Contains(contentType, "charset") {
		contentType += "; charset=utf-8"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Connection", "close")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}

func (s *webServer) notFound(w http.ResponseWriter, r *http.Request) {
	body := fmt.Sprintf(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<title>404 - Not found</title>
<style>
 body{font:16px/1.6 system-ui,sans-serif;background:#0a0f1c;color:#e6edf7;
      display:grid;place-items:center;min-height:100vh;margin:0;text-align:center}
 code{color:#fbbf24} a{color:#7dd3fc}
</style></head>
<body><div>
<h1>404 - Not found</h1>
<p><code>%s</code> does not exist in <code>%s</code>.</p>
<p><a href="/">Back to the index page</a></p>
</div></body></html>`, template.HTMLEscapeString(r.URL.Path), template.HTMLEscapeString(s.root))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Connection", "close")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, body)
}

func (s *webServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	entries, total := s.logs.snapshot()
	payload := fmt.Sprintf(`{"mode":"http","port":%d,"uptime_seconds":%d,"requests_total":%d,"log_entries":%d}`,
		s.port, int(time.Since(s.started).Seconds()), total, len(entries))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	w.Header().Set("Connection", "close")
	_, _ = io.WriteString(w, payload)
}

func (s *webServer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	entries, total := s.logs.snapshot()
	urls := localURLs(s.port)
	data := struct {
		Version string
		Mode    string
		Port    int
		Root    string
		Uptime  string
		Started string
		Total   int
		Entries []requestEntry
		URLs    []string
		GOOS    string
		Public  []string
	}{
		Version: version,
		Mode:    "HTTP",
		Port:    s.port,
		Root:    s.root,
		Uptime:  time.Since(s.started).Round(time.Second).String(),
		Started: s.started.Format("Mon 15:04:05"),
		Total:   total,
		Entries: entries,
		URLs:    urls,
		GOOS:    runtime.GOOS,
		Public:  listPublicFiles(s.root),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Connection", "close")
	if err := s.tmpl.Execute(w, data); err != nil {
		log.Printf("dashboard template error: %v", err)
	}
}

func listPublicFiles(root string) []string {
	var files []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(files)
	if len(files) > 50 {
		files = files[:50]
	}
	return files
}

// localURLs returns the URLs other machines can use to reach this server.
func localURLs(port int) []string {
	urls := []string{fmt.Sprintf("http://localhost:%d/", port)}
	ifaces, err := net.Interfaces()
	if err != nil {
		return urls
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.To4() == nil {
				continue
			}
			urls = append(urls, fmt.Sprintf("http://%s:%d/", ip.String(), port))
		}
	}
	return urls
}

// ---------------------------------------------------------------------------
// Raw TCP mode
// ---------------------------------------------------------------------------

// runRaw listens on a plain TCP socket. There is no HTTP here: whatever bytes
// arrive are printed to the terminal, then a human-readable string is sent back
// and the socket is closed.
func runRaw(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer ln.Close()

	log.Printf("mini-http-server %s (RAW TCP mode) listening on %s", version, addr)
	log.Printf("Connect with:  telnet <this-ip> %s   or   nc <this-ip> %s",
		portOf(addr), portOf(addr))
	log.Printf("Send any text. The server will print it and reply without HTTP headers.")
	log.Printf("Press Ctrl+C to stop.")

	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go handleRawConn(conn)
	}
}

func handleRawConn(conn net.Conn) {
	defer conn.Close()
	peer := conn.RemoteAddr().String()
	log.Printf("--- raw connection from %s ---", peer)

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil && err != io.EOF {
		log.Printf("read error from %s: %v", peer, err)
	}
	received := buf[:n]

	fmt.Printf("%s\n", string(received))
	if len(received) > 0 && received[len(received)-1] != '\n' {
		fmt.Println()
	}
	log.Printf("--- end of input (%d bytes) ---", n)

	reply := "Hello from the Internet 101 raw TCP server!\n" +
		"You sent " + strconv.Itoa(n) + " byte(s).\n" +
		"There were no HTTP headers in this response - just these words.\n"
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, reply); err != nil {
		log.Printf("write error to %s: %v", peer, err)
	}
	log.Printf("--- connection closed: %s ---", peer)
}

func portOf(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "8080"
	}
	return port
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// resolveRoot prefers the public folder in the current directory, but falls
// back to a folder next to the executable. That makes double-clicking the
// Windows .exe behave the way people expect.
func resolveRoot(flagValue string) string {
	if filepath.IsAbs(flagValue) {
		return flagValue
	}
	if info, err := os.Stat(flagValue); err == nil && info.IsDir() {
		return flagValue
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), flagValue)
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return flagValue
}

func ensureIndex(root string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	indexPath := filepath.Join(root, "index.html")
	if _, err := os.Stat(indexPath); err == nil {
		return nil
	}
	log.Printf("no index page found - creating a default one at %s", indexPath)
	return os.WriteFile(indexPath, []byte(defaultIndexHTML), 0o644)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("could not open a browser automatically: %v", err)
		log.Printf("open this URL manually: %s", url)
	}
}

func main() {
	rawMode := flag.Bool("raw", false, "run in raw TCP mode instead of HTTP mode")
	port := flag.Int("port", 8080, "TCP port to listen on")
	host := flag.String("host", "0.0.0.0", "interface to bind (0.0.0.0 = all interfaces)")
	dir := flag.String("dir", "public", "folder containing the static website")
	open := flag.Bool("open", false, "open the default browser after starting (HTTP mode only)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("mini-http-server %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return
	}
	if *port < 1 || *port > 65535 {
		log.Fatalf("invalid port %d: must be between 1 and 65535", *port)
	}

	addr := net.JoinHostPort(*host, strconv.Itoa(*port))
	log.SetFlags(0) // we format our own, clean log lines

	if *rawMode {
		if err := runRaw(addr); err != nil {
			log.Fatalf("raw TCP server failed: %v", err)
		}
		return
	}

	root := resolveRoot(*dir)
	if err := ensureIndex(root); err != nil {
		log.Fatalf("could not prepare public folder %q: %v", root, err)
	}

	srv := newWebServer(root, *port)
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("mini-http-server %s (HTTP mode)", version)
	log.Printf("Serving files from: %s", root)
	log.Printf("Listening on: %s", addr)
	for _, u := range localURLs(*port) {
		log.Printf("  -> %s", u)
	}
	log.Printf("Workshop dashboard: http://localhost:%d/__workshop", *port)
	if runtime.GOOS == "windows" {
		log.Printf("Windows tip: allow this app through the firewall on private networks")
		log.Printf("            if other laptops cannot connect.")
	}
	log.Printf("Press Ctrl+C to stop.")

	if *open {
		go func() {
			time.Sleep(300 * time.Millisecond)
			openBrowser(fmt.Sprintf("http://localhost:%d/", *port))
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server failed: %v", err)
		}
	case <-ctx.Done():
		log.Printf("shutting down...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}
}

// dashboardHTML is a tiny, dependency-free status page for workshop admins.
const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="5">
<title>Internet 101 - Mini HTTP Server</title>
<style>
  :root { color-scheme: dark; }
  * { box-sizing: border-box; }
  body { margin:0; background:#0a0f1c; color:#e6edf7;
         font:15px/1.55 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
  header { padding:1.25rem 1.5rem; border-bottom:1px solid #ffffff14;
           display:flex; flex-wrap:wrap; gap:.75rem 1.5rem; align-items:center; }
  h1 { font-size:1.05rem; margin:0; letter-spacing:.04em; }
  .dot { width:9px; height:9px; border-radius:50%; background:#22c55e;
         display:inline-block; margin-right:.45rem; box-shadow:0 0 10px #22c55e; }
  .muted { color:#8b9bb4; }
  main { padding:1.5rem; display:grid; gap:1.5rem;
         grid-template-columns:repeat(auto-fit, minmax(320px, 1fr)); }
  section { background:#ffffff06; border:1px solid #ffffff12; border-radius:12px;
            padding:1rem 1.1rem; overflow:hidden; }
  h2 { font-size:.72rem; letter-spacing:.14em; text-transform:uppercase;
       color:#8b9bb4; margin:0 0 .8rem; }
  dl { display:grid; grid-template-columns:auto 1fr; gap:.35rem 1rem; margin:0; }
  dt { color:#8b9bb4; }
  dd { margin:0; overflow-wrap:anywhere; }
  a { color:#7dd3fc; text-decoration:none; }
  a:hover { text-decoration:underline; }
  table { width:100%; border-collapse:collapse; font-size:.82rem; }
  th, td { text-align:left; padding:.35rem .4rem; border-bottom:1px solid #ffffff0d;
           white-space:nowrap; }
  th { color:#8b9bb4; font-weight:500; }
  td.path { max-width:280px; overflow:hidden; text-overflow:ellipsis; }
  .ok { color:#4ade80; }
  .warn { color:#fbbf24; }
  .err { color:#f87171; }
  ul.files { margin:0; padding-left:1.1rem; }
  .hint { margin-top:1rem; font-size:.78rem; color:#8b9bb4; }
</style>
</head>
<body>
<header>
  <h1><span class="dot"></span>INTERNET 101 / MINI HTTP SERVER</h1>
  <span class="muted">v{{.Version}} &middot; {{.GOOS}}</span>
  <span class="muted">mode: {{.Mode}}</span>
</header>
<main>
  <section>
    <h2>Server</h2>
    <dl>
      <dt>Uptime</dt><dd>{{.Uptime}}</dd>
      <dt>Started</dt><dd>{{.Started}}</dd>
      <dt>Port</dt><dd>{{.Port}}</dd>
      <dt>Root</dt><dd>{{.Root}}</dd>
      <dt>Requests</dt><dd>{{.Total}}</dd>
    </dl>
    <p class="hint">Open from another device:</p>
    <ul class="files">{{range .URLs}}<li><a href="{{.}}">{{.}}</a></li>{{end}}</ul>
  </section>

  <section>
    <h2>Public files</h2>
    {{if .Public}}
    <ul class="files">{{range .Public}}<li><a href="/{{.}}">{{.}}</a></li>{{end}}</ul>
    {{else}}<p class="muted">No files yet.</p>{{end}}
    <p class="hint">Drop HTML, CSS, JS and images into the public folder.</p>
  </section>

  <section style="grid-column:1/-1">
    <h2>Recent requests (newest first)</h2>
    {{if .Entries}}
    <table>
      <thead><tr><th>Time</th><th>Method</th><th>Path</th><th>Client</th><th>Status</th><th>Bytes</th><th>Took</th></tr></thead>
      <tbody>
      {{range .Entries}}
        <tr>
          <td class="muted">{{.Time}}</td>
          <td>{{.Method}}</td>
          <td class="path">{{.Path}}</td>
          <td>{{.Client}}</td>
          <td class="{{if lt .Status 400}}ok{{else if lt .Status 500}}warn{{else}}err{{end}}">{{.Status}}</td>
          <td>{{.Bytes}}</td>
          <td class="muted">{{.Duration}}</td>
        </tr>
      {{end}}
      </tbody>
    </table>
    {{else}}<p class="muted">No requests yet. Refresh the index page!</p>{{end}}
    <p class="hint">This page refreshes every 5 seconds.</p>
  </section>
</main>
</body>
</html>
`
