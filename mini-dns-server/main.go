// Command mini-dns-server is the "DNS" station of the Internet 101 workshop.
//
// It answers A queries for a set of custom workshop TLDs (".web", ".pi", ...)
// from a thread-safe in-memory registry, always with TTL 0 so that browsers and
// operating systems never cache an answer across a reconfiguration. Everything
// else is forwarded to a real upstream resolver.
//
// It also exposes a small HTTP API on port 8090 so the Domain Portal (or any
// other tool) can register and inspect records:
//
//	GET    /api/records           list every registered record
//	POST   /api/register          {"domain":"alice","ip":"192.168.1.42"}
//	DELETE /api/records?domain=alice.web
//	GET    /api/health            liveness probe
//
// The DNS protocol is implemented directly on top of the standard library, so
// the server has zero third-party dependencies.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const version = "1.0.0"

const (
	typeA    uint16 = 1
	classIN  uint16 = 1
	rcodeOK  int    = 0
	rcodeErr int    = 1 // FORMERR
	rcodeNX  int    = 3 // NXDOMAIN
	rcodeNot int    = 4 // NOTIMP
	rcodeSrv int    = 2 // SERVFAIL
)

// ---------------------------------------------------------------------------
// In-memory registry
// ---------------------------------------------------------------------------

type record struct {
	Domain  string    `json:"domain"`
	IP      string    `json:"ip"`
	Updated time.Time `json:"updated"`
}

type registry struct {
	mu      sync.RWMutex
	records map[string]record
}

func newRegistry() *registry {
	return &registry{records: make(map[string]record)}
}

// lookup returns the IPv4 address registered for a domain.
func (r *registry) lookup(domain string) (net.IP, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.records[strings.ToLower(strings.TrimSuffix(domain, "."))]
	if !ok {
		return nil, false
	}
	return net.ParseIP(rec.IP), true
}

// register creates or replaces a domain -> IPv4 mapping.
func (r *registry) register(domain, ip string) (record, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, existed := r.records[domain]
	rec := record{Domain: domain, IP: ip, Updated: time.Now().UTC()}
	r.records[domain] = rec
	return rec, !existed
}

func (r *registry) remove(domain string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.records[domain]
	delete(r.records, domain)
	return ok
}

func (r *registry) list() []record {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]record, 0, len(r.records))
	for _, rec := range r.records {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out
}

func (r *registry) count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.records)
}

// ---------------------------------------------------------------------------
// DNS wire format helpers
// ---------------------------------------------------------------------------

var errMalformed = errors.New("malformed DNS message")

type dnsQuestion struct {
	name   string
	qtype  uint16
	qclass uint16
	end    int // offset of the byte just after the question section
}

// parseQuestion reads the header and the single question of a DNS query.
func parseQuestion(msg []byte) (id, flags uint16, q dnsQuestion, err error) {
	if len(msg) < 12 {
		return 0, 0, q, errMalformed
	}
	id = binary.BigEndian.Uint16(msg[0:2])
	flags = binary.BigEndian.Uint16(msg[2:4])
	if binary.BigEndian.Uint16(msg[4:6]) != 1 {
		return id, flags, q, fmt.Errorf("%w: expected exactly one question", errMalformed)
	}
	name, next, err := decodeName(msg, 12)
	if err != nil {
		return id, flags, q, err
	}
	if next+4 > len(msg) {
		return id, flags, q, errMalformed
	}
	q.name = name
	q.qtype = binary.BigEndian.Uint16(msg[next : next+2])
	q.qclass = binary.BigEndian.Uint16(msg[next+2 : next+4])
	q.end = next + 4
	return id, flags, q, nil
}

// decodeName decodes a (possibly compressed) DNS name starting at off and
// returns the name plus the offset of the next field in the original stream.
func decodeName(msg []byte, off int) (string, int, error) {
	var labels []string
	next := -1
	jumps := 0

	for {
		if off < 0 || off >= len(msg) {
			return "", 0, errMalformed
		}
		length := int(msg[off])
		switch {
		case length == 0:
			off++
			if next < 0 {
				next = off
			}
			return strings.Join(labels, "."), next, nil

		case length&0xC0 == 0xC0: // compression pointer
			if off+1 >= len(msg) {
				return "", 0, errMalformed
			}
			ptr := int(binary.BigEndian.Uint16(msg[off:off+2]) & 0x3FFF)
			if next < 0 {
				next = off + 2
			}
			if ptr >= len(msg) {
				return "", 0, errMalformed
			}
			off = ptr
			jumps++
			if jumps > 16 {
				return "", 0, fmt.Errorf("%w: compression loop", errMalformed)
			}

		case length&0xC0 != 0: // reserved label type
			return "", 0, errMalformed

		default:
			if off+1+length > len(msg) || length > 63 {
				return "", 0, errMalformed
			}
			labels = append(labels, string(msg[off+1:off+1+length]))
			off += 1 + length
			if len(labels) > 64 {
				return "", 0, errMalformed
			}
		}
	}
}

// buildResponse constructs a DNS response. ip may be nil (NXDOMAIN/NODATA).
// The question is echoed verbatim and every answer record has TTL 0.
func buildResponse(query []byte, q dnsQuestion, id, queryFlags uint16, ip net.IP, rcode int, authoritative bool) []byte {
	var answer []byte
	ancount := uint16(0)

	if ip4 := ip.To4(); ip4 != nil {
		ancount = 1
		answer = make([]byte, 16)
		answer[0], answer[1] = 0xC0, 0x0C // pointer to the name at offset 12
		binary.BigEndian.PutUint16(answer[2:4], typeA)
		binary.BigEndian.PutUint16(answer[4:6], classIN)
		binary.BigEndian.PutUint32(answer[6:10], 0) // TTL: 0 (no caching)
		binary.BigEndian.PutUint16(answer[10:12], 4)
		copy(answer[12:16], ip4)
	}

	questionBytes := query[12:q.end]
	resp := make([]byte, 12, 12+len(questionBytes)+len(answer))

	binary.BigEndian.PutUint16(resp[0:2], id)

	// QR=1, copy the opcode and RD bit, set RA, optionally AA, then the rcode.
	flags := uint16(0x8000) | (queryFlags & 0x7900) | 0x0080
	if authoritative {
		flags |= 0x0400
	}
	flags |= uint16(rcode) & 0x000F
	binary.BigEndian.PutUint16(resp[2:4], flags)

	binary.BigEndian.PutUint16(resp[4:6], 1) // QDCOUNT
	binary.BigEndian.PutUint16(resp[6:8], ancount)
	binary.BigEndian.PutUint16(resp[8:10], 0) // NSCOUNT
	binary.BigEndian.PutUint16(resp[10:12], 0)

	resp = append(resp, questionBytes...)
	resp = append(resp, answer...)
	return resp
}

// ---------------------------------------------------------------------------
// DNS server
// ---------------------------------------------------------------------------

type dnsServer struct {
	reg        *registry
	upstream   string
	tlds       []string
	defaultTLD string
}

func (s *dnsServer) isWorkshopName(name string) bool {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	for _, tld := range s.tlds {
		bare := strings.TrimPrefix(tld, ".")
		if name == bare || strings.HasSuffix(name, tld) {
			return true
		}
	}
	return false
}

func (s *dnsServer) handlePacket(pc net.PacketConn, msg []byte, client net.Addr) {
	start := time.Now()

	id, flags, q, err := parseQuestion(msg)
	if err != nil {
		log.Printf("[DNS] malformed query from %s: %v", client, err)
		if len(msg) >= 12 {
			// Best effort: echo the ID with FORMERR and no question section.
			resp := make([]byte, 12)
			copy(resp[0:2], msg[0:2])
			binary.BigEndian.PutUint16(resp[2:4], uint16(0x8000|0x0080|rcodeErr))
			_, _ = pc.WriteTo(resp, client)
		}
		return
	}

	name := strings.ToLower(strings.TrimSuffix(q.name, "."))

	if !s.isWorkshopName(name) {
		s.forward(pc, msg, client, name, q, start)
		return
	}

	// Workshop domains are authoritative: answer, NODATA, or NXDOMAIN.
	if q.qclass != classIN {
		resp := buildResponse(msg, q, id, flags, nil, rcodeNot, true)
		_, _ = pc.WriteTo(resp, client)
		log.Printf("[DNS] %s class=%d -> NOTIMP (%s)", name, q.qclass, client)
		return
	}

	if q.qtype != typeA {
		resp := buildResponse(msg, q, id, flags, nil, rcodeOK, true)
		_, _ = pc.WriteTo(resp, client)
		log.Printf("[DNS] %s type=%d -> NOERROR/NODATA (only A records) (%s)",
			name, q.qtype, client)
		return
	}

	if ip, ok := s.reg.lookup(name); ok {
		resp := buildResponse(msg, q, id, flags, ip, rcodeOK, true)
		_, _ = pc.WriteTo(resp, client)
		log.Printf("[DNS] %s A -> %s (TTL 0) (%s, %s)",
			name, ip, client, time.Since(start).Round(time.Microsecond))
		return
	}

	resp := buildResponse(msg, q, id, flags, nil, rcodeNX, true)
	_, _ = pc.WriteTo(resp, client)
	log.Printf("[DNS] %s A -> NXDOMAIN (%s)", name, client)
}

// forward relays a query to the upstream resolver and returns the raw answer.
func (s *dnsServer) forward(pc net.PacketConn, msg []byte, client net.Addr, name string, q dnsQuestion, start time.Time) {
	if s.upstream == "" {
		resp := buildResponse(msg, q, binary.BigEndian.Uint16(msg[0:2]), binary.BigEndian.Uint16(msg[2:4]), nil, rcodeSrv, false)
		_, _ = pc.WriteTo(resp, client)
		log.Printf("[DNS] %s -> SERVFAIL (no upstream) (%s)", name, client)
		return
	}

	conn, err := net.DialTimeout("udp", s.upstream, 2*time.Second)
	if err != nil {
		s.replyServfail(pc, msg, q, client, name, err)
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	if _, err := conn.Write(msg); err != nil {
		s.replyServfail(pc, msg, q, client, name, err)
		return
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		s.replyServfail(pc, msg, q, client, name, err)
		return
	}

	_, _ = pc.WriteTo(buf[:n], client)
	log.Printf("[DNS] %s type=%d -> forwarded to %s (%s, %s)",
		name, q.qtype, s.upstream, client, time.Since(start).Round(time.Millisecond))
}

func (s *dnsServer) replyServfail(pc net.PacketConn, msg []byte, q dnsQuestion, client net.Addr, name string, cause error) {
	id := binary.BigEndian.Uint16(msg[0:2])
	flags := binary.BigEndian.Uint16(msg[2:4])
	resp := buildResponse(msg, q, id, flags, nil, rcodeSrv, false)
	_, _ = pc.WriteTo(resp, client)
	log.Printf("[DNS] %s -> SERVFAIL: %v (%s)", name, cause, client)
}

// ---------------------------------------------------------------------------
// HTTP API
// ---------------------------------------------------------------------------

type apiServer struct {
	reg        *registry
	tlds       []string
	defaultTLD string
	upstream   string
}

type apiRecord struct {
	Domain  string `json:"domain"`
	IP      string `json:"ip"`
	Updated string `json:"updated"`
}

func (a *apiServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/records", a.handleRecords)
	mux.HandleFunc("/api/register", a.handleRegister)
	mux.HandleFunc("/api/delete", a.handleDelete)
	mux.HandleFunc("/api/health", a.handleHealth)
	mux.HandleFunc("/", a.handleRoot)
	return withCORS(mux)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		http.Error(w, `{"ok":false,"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	body = append(body, '\n')
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (a *apiServer) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "mini-dns-server",
		"version": version,
		"endpoints": []string{
			"GET /api/records",
			"POST /api/register",
			"DELETE /api/records?domain=name.web",
			"GET /api/health",
		},
	})
}

func (a *apiServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"records": a.reg.count(),
		"tlds":    a.tlds,
	})
}

func (a *apiServer) handleRecords(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		records := a.reg.list()
		out := make([]apiRecord, 0, len(records))
		for _, rec := range records {
			out = append(out, apiRecord{
				Domain:  rec.Domain,
				IP:      rec.IP,
				Updated: rec.Updated.Format(time.RFC3339),
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"records":  out,
			"count":    len(out),
			"tlds":     a.tlds,
			"upstream": a.upstream,
		})

	case http.MethodDelete:
		domain, err := normalizeDomain(r.URL.Query().Get("domain"), a.defaultTLD, a.tlds)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		removed := a.reg.remove(domain)
		log.Printf("[API] delete %s (removed=%v)", domain, removed)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "domain": domain, "removed": removed})

	default:
		w.Header().Set("Allow", "GET, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
	}
}

type registerRequest struct {
	Domain string `json:"domain"`
	IP     string `json:"ip"`
}

func (a *apiServer) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}

	var req registerRequest
	contentType := r.Header.Get("Content-Type")
	if strings.Contains(contentType, "application/json") {
		defer r.Body.Close()
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body: " + err.Error()})
			return
		}
	} else {
		if err := r.ParseForm(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid form body"})
			return
		}
		req.Domain = r.FormValue("domain")
		req.IP = r.FormValue("ip")
	}

	domain, err := normalizeDomain(req.Domain, a.defaultTLD, a.tlds)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	ip := net.ParseIP(strings.TrimSpace(req.IP))
	if ip == nil || ip.To4() == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": "ip must be a valid IPv4 address, for example 192.168.1.42",
		})
		return
	}

	rec, created := a.reg.register(domain, ip.To4().String())
	log.Printf("[API] register %s -> %s (new=%v)", rec.Domain, rec.IP, created)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"created": created,
		"record": apiRecord{
			Domain:  rec.Domain,
			IP:      rec.IP,
			Updated: rec.Updated.Format(time.RFC3339),
		},
	})
}

func (a *apiServer) handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	var req registerRequest
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
	} else {
		_ = r.ParseForm()
		req.Domain = r.FormValue("domain")
	}
	domain, err := normalizeDomain(req.Domain, a.defaultTLD, a.tlds)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	removed := a.reg.remove(domain)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "domain": domain, "removed": removed})
}

// normalizeDomain lowercases, strips a scheme/path if the user pasted a URL,
// appends the default TLD when omitted, and validates every label.
func normalizeDomain(raw, defaultTLD string, tlds []string) (string, error) {
	d := strings.ToLower(strings.TrimSpace(raw))
	d = strings.TrimSuffix(d, ".")

	if i := strings.Index(d, "://"); i >= 0 {
		d = d[i+3:]
	}
	if i := strings.IndexAny(d, "/?#"); i >= 0 {
		d = d[:i]
	}
	if i := strings.Index(d, ":"); i >= 0 {
		d = d[:i]
	}
	if d == "" {
		return "", errors.New("domain is required")
	}
	if !strings.Contains(d, ".") {
		d += defaultTLD
	}
	if len(d) > 253 {
		return "", errors.New("domain is too long")
	}

	labels := strings.Split(d, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return "", fmt.Errorf("invalid domain label in %q", d)
		}
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", fmt.Errorf("domain labels cannot start or end with a hyphen: %q", d)
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", fmt.Errorf("invalid character %q in domain %q", c, d)
			}
		}
	}

	tld := "." + labels[len(labels)-1]
	for _, allowed := range tlds {
		if tld == allowed {
			return d, nil
		}
	}
	return "", fmt.Errorf("domain must end in %s", strings.Join(tlds, " or "))
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func parseTLDs(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	var out []string
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, ".") {
			p = "." + p
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, errors.New("at least one workshop TLD is required")
	}
	return out, nil
}

func main() {
	port := flag.Int("port", 53, "UDP port for DNS (53 needs root or CAP_NET_BIND_SERVICE)")
	host := flag.String("host", "0.0.0.0", "interface to bind the DNS server to")
	apiPort := flag.Int("api-port", 8090, "port for the HTTP management API")
	apiHost := flag.String("api-host", "0.0.0.0", "interface to bind the HTTP API to")
	upstream := flag.String("upstream", "1.1.1.1:53", "upstream DNS server for non-workshop names (empty disables)")
	tldsRaw := flag.String("tlds", ".web,.pi", "comma separated workshop TLDs")
	defaultTLD := flag.String("default-tld", ".web", "TLD appended when a registered name has none")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	log.SetFlags(log.Ltime)

	if *showVersion {
		fmt.Printf("mini-dns-server %s\n", version)
		return
	}
	if *port < 1 || *port > 65535 {
		log.Fatalf("invalid DNS port %d", *port)
	}

	tlds, err := parseTLDs(*tldsRaw)
	if err != nil {
		log.Fatalf("invalid --tlds: %v", err)
	}
	def := strings.ToLower(strings.TrimSpace(*defaultTLD))
	if !strings.HasPrefix(def, ".") {
		def = "." + def
	}
	validDefault := false
	for _, t := range tlds {
		if t == def {
			validDefault = true
		}
	}
	if !validDefault {
		log.Fatalf("--default-tld %q must be one of %s", def, strings.Join(tlds, ", "))
	}

	up := strings.TrimSpace(*upstream)
	if up != "" && !strings.Contains(up, ":") {
		up += ":53"
	}

	reg := newRegistry()
	dnsAddr := net.JoinHostPort(*host, strconv.Itoa(*port))
	apiAddr := net.JoinHostPort(*apiHost, strconv.Itoa(*apiPort))

	dns := &dnsServer{reg: reg, upstream: up, tlds: tlds, defaultTLD: def}
	api := &apiServer{reg: reg, tlds: tlds, defaultTLD: def, upstream: up}

	// The DNS socket must be bound before we announce success. Binding port 53
	// as a normal user is the most common setup mistake, so explain it clearly.
	pc, err := net.ListenPacket("udp", dnsAddr)
	if err != nil {
		if *port < 1024 && os.Geteuid() != 0 {
			log.Fatalf("cannot bind %s: %v\n"+
				"Ports below 1024 require privileges. Either run with sudo:\n"+
				"  sudo ./mini-dns-server\n"+
				"or grant the binary the capability once:\n"+
				"  sudo setcap 'cap_net_bind_service=+ep' ./mini-dns-server\n"+
				"or use a high port for testing:\n"+
				"  ./mini-dns-server --port 5353 --api-port 8090", dnsAddr, err)
		}
		log.Fatalf("cannot bind DNS socket %s: %v", dnsAddr, err)
	}

	httpServer := &http.Server{
		Addr:              apiAddr,
		Handler:           api.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("mini-dns-server %s", version)
	log.Printf("Workshop TLDs: %s (default %s)", strings.Join(tlds, ", "), def)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dnsErr := make(chan error, 1)
	go func() {
		// serveUDP owns the socket; this goroutine just runs the loop.
		dnsErr <- dns.serveUDPConn(pc)
	}()

	apiErr := make(chan error, 1)
	go func() {
		log.Printf("HTTP API listening on http://%s", apiAddr)
		apiErr <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-dnsErr:
		if err != nil {
			log.Fatalf("DNS server stopped: %v", err)
		}
	case err := <-apiErr:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP API stopped: %v", err)
		}
	case <-ctx.Done():
		log.Printf("shutting down...")
	}

	stop()
	_ = pc.Close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}

// serveUDPConn runs the read loop on an already-bound socket.
func (s *dnsServer) serveUDPConn(pc net.PacketConn) error {
	log.Printf("DNS listening on udp://%s (workshop answers always use TTL 0)", pc.LocalAddr())
	if s.upstream != "" {
		log.Printf("Forwarding other names to %s", s.upstream)
	} else {
		log.Printf("Upstream forwarding disabled (unknown names get SERVFAIL)")
	}
	for {
		buf := make([]byte, 4096)
		n, client, err := pc.ReadFrom(buf)
		if err != nil {
			return err
		}
		go s.handlePacket(pc, buf[:n], client)
	}
}
