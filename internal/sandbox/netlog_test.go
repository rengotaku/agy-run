package sandbox

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
)

func newTestProxy(t *testing.T, runID string, out io.Writer) (*Proxy, string) {
	t.Helper()
	p := NewProxy(runID, out)
	addr, err := p.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return p, addr
}

func proxyClient(t *testing.T, addr string, insecure bool) *http.Client {
	t.Helper()
	proxyURL, err := url.Parse("http://" + addr)
	if err != nil {
		t.Fatalf("parse proxy addr: %v", err)
	}
	// DisableKeepAlives: our minimal proxy logs a connection once it closes
	// (one JSONL line per TCP connection, matching a real bwrap sandbox where
	// the connection ends when agy's process exits). A keep-alive client
	// would leave the connection idle-open forever and the test would hang
	// waiting for Proxy.Stop() to observe EOF, so tests force each request
	// onto its own short-lived connection.
	tr := &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true}
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test-only self-signed backend
	}
	return &http.Client{Transport: tr}
}

func TestProxyForwardsPlainHTTPAndLogs(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello-plain"))
	}))
	defer backend.Close()

	var logBuf bytes.Buffer
	p, addr := newTestProxy(t, "run-plain", &logBuf)

	client := proxyClient(t, addr, false)
	resp, err := client.Get(backend.URL)
	if err != nil {
		t.Fatalf("GET via proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "hello-plain" {
		t.Fatalf("body = %q, want hello-plain", body)
	}

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	lines := jsonLines(t, logBuf.String())
	if len(lines) != 1 {
		t.Fatalf("want 1 log line, got %d: %s", len(lines), logBuf.String())
	}
	if lines[0].Method != http.MethodGet {
		t.Errorf("method = %q, want GET", lines[0].Method)
	}
	if lines[0].RunID != "run-plain" {
		t.Errorf("run_id = %q, want run-plain", lines[0].RunID)
	}
	if lines[0].BytesDown == 0 {
		t.Errorf("bytes_down = 0, want > 0")
	}
	if lines[0].TS == "" {
		t.Errorf("ts is empty")
	}
}

func TestProxyTunnelsHTTPSAndLogs(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello-tls"))
	}))
	defer backend.Close()

	var logBuf bytes.Buffer
	p, addr := newTestProxy(t, "run-tls", &logBuf)

	client := proxyClient(t, addr, true)
	resp, err := client.Get(backend.URL)
	if err != nil {
		t.Fatalf("GET via proxy (CONNECT): %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "hello-tls" {
		t.Fatalf("body = %q, want hello-tls", body)
	}

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	lines := jsonLines(t, logBuf.String())
	if len(lines) != 1 {
		t.Fatalf("want 1 log line, got %d: %s", len(lines), logBuf.String())
	}
	e := lines[0]
	if e.Method != http.MethodConnect {
		t.Errorf("method = %q, want CONNECT", e.Method)
	}
	wantHost, wantPortStr, err := net.SplitHostPort(strings.TrimPrefix(backend.URL, "https://"))
	if err != nil {
		t.Fatalf("split backend host:port: %v", err)
	}
	if e.Host != wantHost {
		t.Errorf("host = %q, want %q", e.Host, wantHost)
	}
	if fmt.Sprint(e.Port) != wantPortStr {
		t.Errorf("port = %d, want %s", e.Port, wantPortStr)
	}
	if e.BytesUp == 0 || e.BytesDown == 0 {
		t.Errorf("bytes_up/down = %d/%d, want both > 0", e.BytesUp, e.BytesDown)
	}
}

// TestProxyConcurrentConnectionsProduceValidJSONLines exercises the
// acceptance criterion that parallel slots sharing one net-access.jsonl never
// corrupt each other's lines (mutex-protected single Write per connection).
func TestProxyConcurrentConnectionsProduceValidJSONLines(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	var logBuf bytes.Buffer
	p, addr := newTestProxy(t, "run-concurrent", &logBuf)
	client := proxyClient(t, addr, false)

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			resp, err := client.Get(backend.URL)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	lines := jsonLines(t, logBuf.String())
	if len(lines) != n {
		t.Fatalf("want %d log lines, got %d", n, len(lines))
	}
}

// TestProxyLogsDialErrorWithoutBlocking exercises the "connect refused"
// path: the proxy must answer 502 and still log a well-formed line with the
// error recorded, never blocking or crashing (the whole point of a
// non-blocking, non-filtering proxy).
func TestProxyLogsDialErrorWithoutBlocking(t *testing.T) {
	var logBuf bytes.Buffer
	p, addr := newTestProxy(t, "run-dialerr", &logBuf)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	// Port 1 is not something anything listens on; the connection should be
	// refused immediately, deterministically, without touching the network.
	if _, err := fmt.Fprintf(conn, "CONNECT 127.0.0.1:1 HTTP/1.1\r\nHost: 127.0.0.1:1\r\n\r\n"); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	_ = conn.Close()

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	lines := jsonLines(t, logBuf.String())
	if len(lines) != 1 {
		t.Fatalf("want 1 log line, got %d: %s", len(lines), logBuf.String())
	}
	if lines[0].Error == "" {
		t.Errorf("expected error field to be set on dial failure")
	}
}

func TestOpenLogFileAppendsAcrossOpens(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/nested/net-access.jsonl"

	f1, err := OpenLogFile(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := f1.WriteString("line1\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	f2, err := OpenLogFile(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	if _, err := f2.WriteString("line2\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f2.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "line1\nline2\n" {
		t.Errorf("content = %q, want both lines appended", data)
	}
}

func jsonLines(t *testing.T, s string) []LogEntry {
	t.Helper()
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []LogEntry
	for _, ln := range strings.Split(s, "\n") {
		var e LogEntry
		if err := json.Unmarshal([]byte(ln), &e); err != nil {
			t.Fatalf("corrupt json line %q: %v", ln, err)
		}
		out = append(out, e)
	}
	return out
}
