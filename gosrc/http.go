// Raw-TCP HTTP/1.1 server: no net/http, no framework. One goroutine per
// connection, keep-alive by default, sequential requests per connection.
// Requests are parsed from a buffered reader with minimal allocation;
// responses are built into one []byte and written with a single Write.
package main

import (
	"bufio"
	"bytes"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
)

const maxBodyBytes = 8 << 20
const maxHeaderBytes = 64 << 10

type request struct {
	method string
	// path is the raw decoded path (no query), split into segments.
	segments []string
	// rawQuery is the query string without '?', or "".
	rawQuery string
	// headerBlock is the raw header bytes (without the request line and
	// without the trailing blank line), lowercased names preserved as sent.
	headerBlock []byte
	body          []byte
	close         bool // Connection: close
	contentLength int
	chunked       bool
	// scratch for segment decoding (reused per connection)
	segBuf [64]string
}

// header returns the value of a header (case-insensitive name), or "".
// Linear scan over the raw block: a handful of headers, cheaper than a map.
func (r *request) header(name string) string {
	h := r.headerBlock
	for len(h) > 0 {
		// find end of line
		i := bytes.IndexByte(h, '\n')
		var line []byte
		if i < 0 {
			line, h = h, nil
		} else {
			line, h = h[:i], h[i+1:]
		}
		line = bytes.TrimRight(line, "\r")
		colon := bytes.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		if len(line) >= len(name) && strings.EqualFold(string(line[:colon]), name) {
			v := line[colon+1:]
			return string(bytes.Trim(v, " \t"))
		}
	}
	return ""
}

// query returns the decoded value of a query parameter, or "".
func (r *request) query(name string) string {
	q := r.rawQuery
	for len(q) > 0 {
		amp := strings.IndexByte(q, '&')
		var pair string
		if amp < 0 {
			pair, q = q, ""
		} else {
			pair, q = q[:amp], q[amp+1:]
		}
		eq := strings.IndexByte(pair, '=')
		var k, v string
		if eq < 0 {
			k, v = pair, ""
		} else {
			k, v = pair[:eq], pair[eq+1:]
		}
		if k == name {
			return unescape(v)
		}
	}
	return ""
}

func unescape(s string) string {
	if !strings.ContainsAny(s, "%+") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			b.WriteByte(hexVal(s[i+1])<<4 | hexVal(s[i+2]))
			i += 2
		} else if c == '+' {
			b.WriteByte(' ')
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

// splitPathSegments splits a decoded path into segments.
func splitPathSegments(path string) []string {
	if path == "" || path == "/" {
		return nil
	}
	// trim leading/trailing slashes
	for len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}
	for len(path) > 0 && path[len(path)-1] == '/' {
		path = path[:len(path)-1]
	}
	if path == "" {
		return nil
	}
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = unescape(s)
	}
	return segs
}

type server struct {
	store *Store
	token string // expected bearer token; "" means accept any
	ln    net.Listener
}

func newServer(store *Store) *server {
	return &server{store: store, token: os.Getenv("MAKEFUN_SHOP_TOKEN")}
}

func (s *server) listen() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	s.ln = ln
	addr := ln.Addr().(*net.TCPAddr)
	return addr.Port, nil
}

func (s *server) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		// Nagle + delayed ACK can stall small request/response ping-pongs;
		// disable Nagle on every accepted connection.
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
		}
		go s.handleConn(conn)
	}
}

func (s *server) close() {
	if s.ln != nil {
		s.ln.Close()
	}
}

// handleConn serves sequential requests on one keep-alive connection.
func (s *server) handleConn(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 8192)
	var scratch []byte
	for {
		req, err := readRequest(br)
		if err != nil {
			return
		}
		scratch = s.dispatch(conn, req, scratch[:0])
		if req.close {
			return
		}
	}
}

// readRequest parses one HTTP/1.1 request from br.
func readRequest(br *bufio.Reader) (*request, error) {
	// request line
	line, err := readLine(br)
	if err != nil {
		return nil, err
	}
	// tolerate a stray blank line before the request line (robustness)
	for len(line) == 0 {
		line, err = readLine(br)
		if err != nil {
			return nil, err
		}
	}
	sp1 := bytes.IndexByte(line, ' ')
	sp2 := bytes.LastIndexByte(line, ' ')
	if sp1 < 0 || sp2 <= sp1 {
		return nil, errBadRequest
	}
	method := string(line[:sp1])
	target := string(line[sp1+1 : sp2])
	req := &request{method: method}
	// split target into path + query
	if q := strings.IndexByte(target, '?'); q >= 0 {
		req.rawQuery = target[q+1:]
		target = target[:q]
	}
	// absolute-form targets (http://host/path) from proxies: strip origin
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		if i := strings.Index(target[8:], "/"); i >= 0 {
			target = target[8+i:]
		} else {
			target = "/"
		}
	}
	req.segments = splitPathSegments(target)

	// headers
	var hb []byte
	total := 0
	for {
		hline, err := readLine(br)
		if err != nil {
			return nil, err
		}
		total += len(hline) + 2
		if total > maxHeaderBytes {
			return nil, errBadRequest
		}
		if len(hline) == 0 {
			break
		}
		hb = append(hb, hline...)
		hb = append(hb, '\n')
	}
	req.headerBlock = hb

	cl := req.header("content-length")
	if cl != "" {
		n, err := strconv.Atoi(strings.TrimSpace(cl))
		if err != nil || n < 0 || n > maxBodyBytes {
			return nil, errBadRequest
		}
		req.contentLength = n
	}
	if strings.EqualFold(req.header("transfer-encoding"), "chunked") {
		req.chunked = true
	}
	connHdr := req.header("connection")
	if strings.EqualFold(connHdr, "close") {
		req.close = true
	}

	// body
	if req.chunked {
		body, err := readChunked(br)
		if err != nil {
			return nil, err
		}
		req.body = body
	} else if req.contentLength > 0 {
		body := make([]byte, req.contentLength)
		if _, err := readFull(br, body); err != nil {
			return nil, err
		}
		req.body = body
	}
	return req, nil
}

func readChunked(br *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		line, err := readLine(br)
		if err != nil {
			return nil, err
		}
		// chunk size line may carry extensions after ';'
		if semi := bytes.IndexByte(line, ';'); semi >= 0 {
			line = line[:semi]
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(line)), 16, 64)
		if err != nil || n < 0 || n > maxBodyBytes {
			return nil, errBadRequest
		}
		if n == 0 {
			// trailers until blank line
			for {
				t, err := readLine(br)
				if err != nil {
					return nil, err
				}
				if len(t) == 0 {
					break
				}
			}
			return out, nil
		}
		chunk := make([]byte, n)
		if _, err := readFull(br, chunk); err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		if len(out) > maxBodyBytes {
			return nil, errBadRequest
		}
		// trailing CRLF after chunk
		if _, err := readLine(br); err != nil {
			return nil, err
		}
	}
}

var errBadRequest = &httpError{400, "Bad Request"}

// readLine reads one line ending in \n, returning it without \r\n.
func readLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	line = bytes.TrimSuffix(line, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	return line, nil
}

// readFull reads exactly len(p) bytes.
func readFull(br *bufio.Reader, p []byte) (int, error) {
	total := 0
	for total < len(p) {
		n, err := br.Read(p[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

// ---- response writing ---------------------------------------------------

// resp is a response under construction: status line + headers + body in
// one buffer, written with a single Write.
type resp struct {
	buf []byte
}

func newResp(status int, contentType string, body []byte, extra ...string) []byte {
	var b []byte
	b = append(b, "HTTP/1.1 "...)
	b = append(b, strconv.Itoa(status)...)
	b = append(b, ' ')
	b = append(b, statusText(status)...)
	b = append(b, "\r\ncontent-type: "...)
	b = append(b, contentType...)
	b = append(b, "\r\ncontent-length: "...)
	b = append(b, strconv.Itoa(len(body))...)
	b = append(b, "\r\nx-github-request-id: "...)
	b = append(b, requestID()...)
	b = append(b, "\r\n"...)
	for i := 0; i+1 < len(extra); i += 2 {
		b = append(b, extra[i]...)
		b = append(b, ": "...)
		b = append(b, extra[i+1]...)
		b = append(b, "\r\n"...)
	}
	b = append(b, "\r\n"...)
	b = append(b, body...)
	return b
}

var reqIDCounter uint64

func requestID() string {
	n := atomic.AddUint64(&reqIDCounter, 1)
	return "req-" + strconv.FormatUint(n, 10)
}

func statusText(status int) string {
	switch status {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 202:
		return "Accepted"
	case 204:
		return "No Content"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 409:
		return "Conflict"
	case 422:
		return "Unprocessable Entity"
	default:
		return "Status"
	}
}

const jsonType = "application/json; charset=utf-8"

// jsonResp builds a JSON response.
func jsonResp(status int, body []byte, extra ...string) []byte {
	return newResp(status, jsonType, body, extra...)
}

// errResp builds a {"message": ...} error response.
func errResp(status int, msg string) []byte {
	body := []byte(`{"message":` + strconv.Quote(msg) + `}`)
	return jsonResp(status, body)
}
