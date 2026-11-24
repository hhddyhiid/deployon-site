// Small personal web server.
// Serves a page and a bidirectional WebSocket carrier endpoint.
package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

const magic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// nodeID identifies this server to the carrier handshake.
const nodeID = "543b499b21674fa6b7bd2924d59fcd9a"

// idBytes is the 16-byte form of nodeID.
var idBytes = decodeHex(nodeID)

func decodeHex(s string) []byte {
	b := make([]byte, 16)
	for i := 0; i < 16; i++ {
		var v byte
		for k := 0; k < 2; k++ {
			c := s[i*2+k]
			switch {
			case c >= '0' && c <= '9':
				v = v<<4 | (c - '0')
			case c >= 'a' && c <= 'f':
				v = v<<4 | (c - 'a' + 10)
			case c >= 'A' && c <= 'F':
				v = v<<4 | (c - 'A' + 10)
			}
		}
		b[i] = v
	}
	return b
}

var started = time.Now()

// debugf writes only when DEBUG is set; the deployment runs silent.
func debugf(format string, args ...any) {
	if os.Getenv("DEBUG") == "" {
		return
	}
	log.Printf(format, args...)
}

const page = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>notes</title>
<style>
  :root { color-scheme: light dark }
  body { font: 16px/1.65 -apple-system, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
         max-width: 34rem; margin: 4rem auto; padding: 0 1.25rem; }
  h1 { font-size: 1.35rem; font-weight: 600; letter-spacing: .01em }
  p  { opacity: .8 }
  footer { margin-top: 3rem; font-size: .8rem; opacity: .55 }
</style>
</head>
<body>
  <h1>Notes</h1>
  <p>A small corner of the internet that keeps a few lines of text and not much else.</p>
  <p>Written slowly, on evenings when there is time to read them back.</p>
  <p id="t"></p>
  <footer>Last build <span id="b">—</span></footer>
<script>
  const d = new Date();
  document.getElementById("t").textContent =
    "Local time here: " + d.toLocaleString("en-GB", { hour12: false });
  document.getElementById("b").textContent = d.toISOString().slice(0, 10);
</script>
</body>
</html>
`

func main() {
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(60)
	}
	debug.SetMemoryLimit(48 << 20)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, page)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "site/1.1")
	})
	mux.HandleFunc("/ip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, outbound())
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintf(w, `{"uptime_s":%d,"goroutines":%d,"heap_mb":%.2f,"sys_mb":%.2f,"num_gc":%d}`,
			int(time.Since(started).Seconds()), runtime.NumGoroutine(),
			float64(m.HeapAlloc)/(1<<20), float64(m.Sys)/(1<<20), m.NumGC)
	})
	mux.HandleFunc("/netcheck", func(w http.ResponseWriter, r *http.Request) {
		targets := []string{"1.1.1.1:443", "8.8.8.8:53", "www.google.com:443", "api.ipify.org:443"}
		type res struct {
			Target string `json:"target"`
			OK     bool   `json:"ok"`
			MS     int64  `json:"ms"`
		}
		out := make([]res, 0, len(targets))
		for _, t := range targets {
			t0 := time.Now()
			c, err := net.DialTimeout("tcp", t, 5*time.Second)
			ms := time.Since(t0).Milliseconds()
			if err == nil {
				c.Close()
			}
			out = append(out, res{Target: t, OK: err == nil, MS: ms})
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		b, _ := json.Marshal(out)
		w.Write(b)
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.NotFound(w, r)
			return
		}
		serveWS(w, r)
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.NotFound(w, r)
			return
		}
		serveWS(w, r)
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	s := &http.Server{
		Addr:              "0.0.0.0:" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if err := s.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// outbound reports the address this process appears as to the outside world.
func outbound() string {
	sources := []string{
		"https://api.ipify.org",
		"https://ipv4.icanhazip.com",
		"http://ip-api.com/line/?fields=query",
		"https://ipinfo.io/ip",
		"https://www.cloudflare.com/cdn-cgi/trace",
	}
	out := make([]string, 0, len(sources))
	cli := &http.Client{Timeout: 4 * time.Second}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, u := range sources {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			v := fetchOne(cli, u)
			if v == "" {
				return
			}
			mu.Lock()
			out = append(out, v)
			mu.Unlock()
		}(u)
	}
	wg.Wait()
	uniq := make([]string, 0, len(out))
	seen := map[string]bool{}
	for _, v := range out {
		if !seen[v] {
			seen[v] = true
			uniq = append(uniq, v)
		}
	}
	primary := ""
	if len(uniq) > 0 {
		primary = uniq[0]
	}
	var b strings.Builder
	b.WriteString(`{"ip":"` + primary + `","sources":[`)
	for i, v := range uniq {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"` + v + `"`)
	}
	b.WriteString(`],"info":`)
	info := fetchText(cli, "http://ip-api.com/json/?fields=status,country,countryCode,regionName,city,isp,org,as,query,hosting")
	if info == "" {
		info = "{}"
	}
	b.WriteString(info)
	b.WriteString("}")
	return b.String()
}

func fetchOne(cli *http.Client, u string) string {
	resp, err := cli.Get(u)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return ""
	}
	txt := strings.TrimSpace(string(body))
	if strings.Contains(u, "cdn-cgi/trace") {
		for _, line := range strings.Split(txt, "\n") {
			if strings.HasPrefix(line, "ip=") {
				return strings.TrimPrefix(line, "ip=")
			}
		}
		return ""
	}
	if len(txt) > 64 || strings.ContainsAny(txt, " <>\"") {
		return ""
	}
	return txt
}

func fetchText(cli *http.Client, u string) string {
	resp, err := cli.Get(u)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return ""
	}
	txt := strings.TrimSpace(string(body))
	if !strings.HasPrefix(txt, "{") {
		return ""
	}
	return txt
}

// ---- WebSocket ----

type conn struct {
	c  net.Conn
	br *bufio.Reader
	wm sync.Mutex
}

func serveWS(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no", http.StatusInternalServerError)
		return
	}
	c, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	sum := sha1.Sum([]byte(key + magic))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := rw.WriteString(resp); err != nil {
		c.Close()
		return
	}
	if err := rw.Flush(); err != nil {
		c.Close()
		return
	}
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	ws := &conn{c: c, br: rw.Reader}
	defer ws.c.Close()

	if r.URL.Path == "/echo" {
		ws.echo(0x2, nil)
		return
	}

	// Every carrier session opens with one frame carrying its handshake.
	op, payload, err := ws.read()
	if err != nil {
		return
	}
	if cmd, host, port, rest, ok := parseOpen(payload); ok && (op == 0x2 || op == 0x1) {
		ws.carrier(cmd, host, port, rest)
	}
}

func (w *conn) echo(op byte, first []byte) {
	w.write(0x1, []byte(`{"hello":"ready"}`))
	if len(first) > 0 {
		if err := w.write(op, first); err != nil {
			return
		}
	}
	for {
		o, payload, err := w.read()
		if err != nil {
			return
		}
		switch o {
		case 0x1, 0x2, 0x0:
			if err := w.write(o, payload); err != nil {
				return
			}
		case 0x8:
			w.write(0x8, nil)
			return
		case 0x9:
			w.write(0xA, payload)
		default:
			return
		}
	}
}

// parseOpen reads the opening frame of a carrier session.
func parseOpen(b []byte) (cmd byte, host string, port int, rest []byte, ok bool) {
	if len(b) < 18 || b[0] != 0 {
		return 0, "", 0, nil, false
	}
	want := idBytes
	if string(b[1:17]) != string(want) {
		return 0, "", 0, nil, false
	}
	o := 18 + int(b[17])
	if len(b) < o+4 {
		return 0, "", 0, nil, false
	}
	cmd = b[o]
	port = int(binary.BigEndian.Uint16(b[o+1:]))
	at := b[o+3]
	i := o + 4
	switch at {
	case 1:
		if len(b) < i+4 {
			return 0, "", 0, nil, false
		}
		host = net.IP(b[i : i+4]).String()
		i += 4
	case 2:
		if len(b) < i+1 {
			return 0, "", 0, nil, false
		}
		n := int(b[i])
		if len(b) < i+1+n {
			return 0, "", 0, nil, false
		}
		host = string(b[i+1 : i+1+n])
		i += 1 + n
	case 3:
		if len(b) < i+16 {
			return 0, "", 0, nil, false
		}
		host = net.IP(b[i : i+16]).String()
		i += 16
	default:
		return 0, "", 0, nil, false
	}
	return cmd, host, port, b[i:], true
}

func (w *conn) carrier(cmd byte, host string, port int, rest []byte) {
	if cmd != 1 {
		// datagram sessions are not carried here
		return
	}
	target, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(port)), 10*time.Second)
	if err != nil {
		debugf("dial %s:%d: %v", host, port, err)
		return
	}
	defer target.Close()
	if tc, ok := target.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	if err := w.write(0x2, []byte{0, 0}); err != nil {
		return
	}
	if len(rest) > 0 {
		if _, err := target.Write(rest); err != nil {
			return
		}
	}
	done := make(chan struct{}, 2)

	go func() { // outgoing
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 16<<10)
		for {
			n, err := target.Read(buf)
			if n > 0 {
				if werr := w.write(0x2, buf[:n]); werr != nil {
					debugf("write ws: %v", werr)
					return
				}
			}
			if err != nil {
				debugf("read target: %v", err)
				w.write(0x8, nil)
				return
			}
		}
	}()

	go func() { // incoming
		defer func() { done <- struct{}{} }()
		for {
			o, payload, err := w.read()
			if err != nil {
				debugf("read ws: %v", err)
				return
			}
			switch o {
			case 0x0, 0x1, 0x2:
				if len(payload) > 0 {
					if _, werr := target.Write(payload); werr != nil {
						debugf("write target: %v", werr)
						return
					}
				}
			case 0x8:
				return
			case 0x9:
				w.write(0xA, payload)
			}
		}
	}()

	<-done
	w.c.SetReadDeadline(time.Now())
}

func (w *conn) write(op byte, payload []byte) error {
	var head [10]byte
	head[0] = 0x80 | op
	n := len(payload)
	var h int
	switch {
	case n < 126:
		head[1] = byte(n)
		h = 2
	case n < 65536:
		head[1] = 126
		binary.BigEndian.PutUint16(head[2:], uint16(n))
		h = 4
	default:
		head[1] = 127
		binary.BigEndian.PutUint64(head[2:], uint64(n))
		h = 10
	}
	w.wm.Lock()
	defer w.wm.Unlock()
	w.c.SetWriteDeadline(time.Now().Add(60 * time.Second))
	buf := make([]byte, 0, h+n)
	buf = append(buf, head[:h]...)
	buf = append(buf, payload...)
	_, err := w.c.Write(buf)
	return err
}

var errProto = errors.New("bad frame")

func (w *conn) read() (byte, []byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(w.br, hdr[:]); err != nil {
		return 0, nil, err
	}
	fin := hdr[0]&0x80 != 0
	op := hdr[0] & 0x0f
	masked := hdr[1]&0x80 != 0
	n := int64(hdr[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(w.br, b[:]); err != nil {
			return 0, nil, err
		}
		n = int64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(w.br, b[:]); err != nil {
			return 0, nil, err
		}
		n = int64(binary.BigEndian.Uint64(b[:]))
	}
	if n < 0 || n > 1<<20 {
		return 0, nil, errProto
	}
	var mk [4]byte
	if masked {
		if _, err := io.ReadFull(w.br, mk[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, n)
	if n > 0 {
		if _, err := io.ReadFull(w.br, payload); err != nil {
			return 0, nil, err
		}
	}
	if masked {
		for i := range payload {
			payload[i] ^= mk[i%4]
		}
	}
	if !fin {
		for {
			o, p, err := w.read()
			if err != nil {
				return 0, nil, err
			}
			payload = append(payload, p...)
			if o != 0x0 || len(payload) > 1<<20 {
				return o, payload, nil
			}
		}
	}
	return op, payload, nil
}
