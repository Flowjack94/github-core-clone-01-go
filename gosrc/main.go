// Lifecycle: bind an ephemeral port, register it with the env, poll /done,
// exit 0 when the trial is over. Mirrors the starter's main.ts exactly.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "[entry] MAKEFUN_ENV_URL is not set: there is no env to register with")
		os.Exit(1)
	}
	store := newStore()
	srv := newServer(store)
	port, err := srv.listen()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[entry] listen: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stdout, "[entry] listening on port %d\n", port)
	go srv.serve()

	if err := register(envURL, port); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] register: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "[entry] registered; waiting for the env to say done")
	waitForDone(envURL)
	srv.close()
	fmt.Fprintln(os.Stdout, "[entry] done")
	os.Exit(0)
}

func register(envURL string, port int) error {
	body, _ := json.Marshal(map[string]int{"port": port})
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest("POST", envURL+"/register", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("content-type", "application/json")
		client := &http.Client{Timeout: 30 * time.Second}
		res, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode == 200 || res.StatusCode == 409 {
				return nil
			}
			if res.StatusCode == 400 || res.StatusCode == 405 {
				return fmt.Errorf("register answered HTTP %d", res.StatusCode)
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

func waitForDone(envURL string) {
	client := &http.Client{Timeout: 30 * time.Second}
	for attempt := 0; ; attempt++ {
		done := false
		res, err := client.Get(envURL + "/done")
		if err == nil {
			if res.StatusCode == 200 {
				var v struct {
					Done bool `json:"done"`
				}
				if json.NewDecoder(res.Body).Decode(&v) == nil && v.Done {
					done = true
				}
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
		if done {
			return
		}
		time.Sleep(retryDelay(attempt))
	}
}

func retryDelay(attempt int) time.Duration {
	if attempt > 8 {
		attempt = 8
	}
	d := 250 * time.Millisecond * time.Duration(1<<uint(attempt))
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	return d
}

// ---- dispatch -----------------------------------------------------------

// dispatch routes one request and writes one response. scratch is a reusable
// buffer; the returned slice is the next scratch (unused by callers).
func (s *server) dispatch(conn net.Conn, req *request, scratch []byte) []byte {
	respBytes := s.route(req)
	_, _ = conn.Write(respBytes)
	return scratch
}

func (s *server) route(req *request) []byte {
	segs := req.segments
	m := req.method

	// GET /healthz: no auth, 200 {"ok":true}
	if m == "GET" && len(segs) == 1 && segs[0] == "healthz" {
		return jsonResp(200, []byte(`{"ok":true}`))
	}

	// Git smart HTTP: /{owner}/{repo}.git/...
	if len(segs) >= 2 && strings.HasSuffix(segs[1], ".git") {
		if !s.authorized(req) {
			return errResp(401, "Requires authentication")
		}
		return s.routeGit(req, segs[0], strings.TrimSuffix(segs[1], ".git"), segs[2:])
	}

	// GraphQL: POST /api/graphql
	if len(segs) == 2 && segs[0] == "api" && segs[1] == "graphql" {
		if m != "POST" {
			return errResp(405, "Method Not Allowed")
		}
		if !s.authorized(req) {
			return jsonResp(401, []byte(`{"message":"Requires authentication"}`))
		}
		return s.routeGraphQL(req)
	}

	// MCP: /mcp
	if len(segs) == 1 && segs[0] == "mcp" {
		return s.routeMCP(req)
	}

	// REST v3: /api/v3/...
	if len(segs) >= 2 && segs[0] == "api" && segs[1] == "v3" {
		return s.routeREST(req, segs[2:])
	}

	// Unknown route: JSON 404 (with request id; auth not required to learn
	// a route does not exist, matching the starter).
	return errResp(404, "Not Found")
}

// authorized checks the bearer token when one is configured.
func (s *server) authorized(req *request) bool {
	if s.token == "" {
		return true
	}
	auth := req.header("authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	return auth[7:] == s.token
}

// requireAuth answers 401 unless the request carries the token.
func (s *server) requireAuth(req *request) []byte {
	if s.authorized(req) {
		return nil
	}
	return errResp(401, "Requires authentication")
}

// parseID converts a decimal string to int, or -1.
func parseID(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// jsonBody parses the request body as JSON into v (encoding/json).
func jsonBody(req *request, v interface{}) error {
	if len(req.body) == 0 {
		return fmt.Errorf("empty body")
	}
	return json.Unmarshal(req.body, v)
}
