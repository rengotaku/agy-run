package sandbox

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// LogEntry is one recorded connection. Proxy writes exactly one JSON line per
// connection (CONNECT tunnel or plain-HTTP forward) to the shared log stream.
type LogEntry struct {
	TS        string  `json:"ts"`
	RunID     string  `json:"run_id,omitempty"`
	Method    string  `json:"method"`
	Host      string  `json:"host"`
	Port      int     `json:"port,omitempty"`
	BytesUp   int64   `json:"bytes_up"`
	BytesDown int64   `json:"bytes_down"`
	DurationS float64 `json:"duration_s"`
	Error     string  `json:"error,omitempty"`
}

// Proxy is a CONNECT-tunnel + plain-HTTP forward proxy that only records what
// passed through — it never inspects payloads and never refuses a request on
// policy grounds. A dial failure is still logged (with the error field set)
// and reported to the client as a normal gateway error; it never blocks or
// crashes the sandboxed process. Proxy is safe for concurrent connections:
// every log line is written under a mutex as a single io.Writer.Write call so
// lines from parallel connections never interleave.
type Proxy struct {
	// RunID identifies the agy-run invocation that owns this proxy instance,
	// so lines from concurrent slots sharing one log file can be told apart.
	RunID string

	now func() time.Time

	mu  sync.Mutex
	out io.Writer

	ln net.Listener
	wg sync.WaitGroup
}

// NewProxy returns a Proxy that appends one JSON line per connection to out.
// out does not need to be safe for concurrent writers itself — Proxy
// serializes every write with its own mutex.
func NewProxy(runID string, out io.Writer) *Proxy {
	return &Proxy{RunID: runID, out: out, now: time.Now}
}

// OpenLogFile opens (creating parent dirs and the file as needed) the shared
// net-access.jsonl log in append mode, so every sandboxed run — across
// concurrency slots — writes to the same stream (all runs unify into one
// file, per issue #11).
func OpenLogFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
}

// Start listens on 127.0.0.1:0 and begins serving in the background. It
// returns the "host:port" the sandboxed process's HTTP(S)_PROXY env vars
// should point at.
func (p *Proxy) Start() (addr string, err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	p.ln = ln
	p.wg.Add(1)
	go p.serve()
	return ln.Addr().String(), nil
}

// Stop closes the listener and waits for every in-flight connection to
// finish (and its log line to be written) before returning.
func (p *Proxy) Stop() error {
	if p.ln == nil {
		return nil
	}
	err := p.ln.Close()
	p.wg.Wait()
	return err
}

func (p *Proxy) serve() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return // listener closed (Stop) or a fatal accept error; stop serving either way
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.handleConn(c)
		}()
	}
}

func (p *Proxy) handleConn(c net.Conn) {
	defer func() { _ = c.Close() }()
	start := p.now()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return // not a well-formed HTTP request; nothing to forward or log
	}
	if req.Method == http.MethodConnect {
		p.handleConnect(c, br, req, start)
		return
	}
	p.handleForward(c, req, start)
}

// handleConnect implements the HTTPS tunnel path: dial the target once,
// answer "200 Connection Established", then relay bytes verbatim in both
// directions (the whole point of CONNECT is that the proxy never looks
// inside the encrypted stream).
func (p *Proxy) handleConnect(c net.Conn, br *bufio.Reader, req *http.Request, start time.Time) {
	target := req.Host
	if target == "" {
		target = req.URL.Host
	}
	host, port := splitHostPort(target)

	dst, err := net.Dial("tcp", target)
	if err != nil {
		_, _ = c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		p.log(LogEntry{Method: req.Method, Host: host, Port: port, Error: err.Error()}, start)
		return
	}
	defer func() { _ = dst.Close() }()

	if _, werr := c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); werr != nil {
		p.log(LogEntry{Method: req.Method, Host: host, Port: port, Error: werr.Error()}, start)
		return
	}

	up, down := relay(br, c, dst)
	p.log(LogEntry{Method: req.Method, Host: host, Port: port, BytesUp: up, BytesDown: down}, start)
}

// handleForward implements the plain-HTTP path: the client sends a full
// absolute-URI request line directly to us (standard forward-proxy
// protocol), we dial the target and re-issue an origin-form request, then
// copy the response back untouched.
func (p *Proxy) handleForward(c net.Conn, req *http.Request, start time.Time) {
	host := req.URL.Hostname()
	if host == "" {
		host = req.Host
	}
	portStr := req.URL.Port()
	if portStr == "" {
		portStr = "80"
	}
	target := net.JoinHostPort(host, portStr)
	_, port := splitHostPort(target)

	dst, err := net.Dial("tcp", target)
	if err != nil {
		_, _ = c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		p.log(LogEntry{Method: req.Method, Host: host, Port: port, Error: err.Error()}, start)
		return
	}
	defer func() { _ = dst.Close() }()

	// RequestURI must be cleared before Write reuses this Request as an
	// outbound one; Write then reconstructs an origin-form request line
	// ("GET /path HTTP/1.1") from req.URL/req.Host, which is what an origin
	// server expects (it is not itself a proxy).
	req.RequestURI = ""
	req.Header.Del("Proxy-Connection")
	req.Header.Del("Proxy-Authorization")

	up := &countWriter{w: dst}
	if werr := req.Write(up); werr != nil {
		p.log(LogEntry{Method: req.Method, Host: host, Port: port, BytesUp: up.n, Error: werr.Error()}, start)
		return
	}
	down, _ := io.Copy(c, dst)
	p.log(LogEntry{Method: req.Method, Host: host, Port: port, BytesUp: up.n, BytesDown: down}, start)
}

// relay copies bytes bidirectionally between the client (br wraps c so any
// bytes already buffered from reading the CONNECT request are not dropped)
// and dst, half-closing each side's write end on EOF so the other direction
// can still drain before the whole connection tears down. It returns bytes
// sent to dst (up) and bytes returned to the client (down).
func relay(br *bufio.Reader, c net.Conn, dst net.Conn) (up, down int64) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		up, _ = io.Copy(dst, br)
		closeWrite(dst)
	}()
	go func() {
		defer wg.Done()
		down, _ = io.Copy(c, dst)
		closeWrite(c)
	}()
	wg.Wait()
	return
}

// closeWrite half-closes the write side of a TCP connection so the peer sees
// EOF without the read side being torn down too. Non-TCP conns are left
// alone; the caller's deferred Close() still reclaims them.
func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
}

// countWriter wraps an io.Writer to count bytes written, so handleForward can
// report bytes_up without a second buffering pass.
type countWriter struct {
	w io.Writer
	n int64
}

func (cw *countWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n += int64(n)
	return n, err
}

// splitHostPort splits "host:port" into host and a numeric port (0 if the
// input has no valid port, e.g. it was already bare).
func splitHostPort(target string) (string, int) {
	h, p, err := net.SplitHostPort(target)
	if err != nil {
		return target, 0
	}
	n, _ := strconv.Atoi(p)
	return h, n
}

// log finalizes and writes one LogEntry as a single JSON line: ts/run_id/
// duration are filled in here so every call site only supplies what it knows
// (method/host/port/bytes/error). The whole marshalled line is written with
// exactly one Write call under p.mu, so concurrent connections sharing one
// output stream never interleave partial lines.
func (p *Proxy) log(e LogEntry, start time.Time) {
	e.TS = start.Format(time.RFC3339)
	e.RunID = p.RunID
	e.DurationS = p.now().Sub(start).Seconds()

	b, err := json.Marshal(e)
	if err != nil {
		return // impossible for this shape, but never let logging crash the run
	}
	b = append(b, '\n')

	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = p.out.Write(b)
}
