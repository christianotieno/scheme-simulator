package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const acceptedResponse = "RESPONSE|ACCEPTED|Transaction processed"

// startTestServer starts a Server on an ephemeral port and registers cleanup.
func startTestServer(t *testing.T, grace time.Duration) *Server {
	t.Helper()
	s := NewServer("127.0.0.1:0", grace)
	if err := s.Start(); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return s
}

// dial opens a connection to the server and registers cleanup.
func dial(t *testing.T, s *Server) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatalf("Dial(%q) = %v", s.Addr(), err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// request writes one framed request and reads one framed response.
func request(t *testing.T, conn net.Conn, r *bufio.Reader, req string) string {
	t.Helper()
	if _, err := io.WriteString(conn, req+"\n"); err != nil {
		t.Fatalf("write %q: %v", req, err)
	}
	resp, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read response to %q: %v", req, err)
	}
	return strings.TrimRight(resp, "\r\n")
}

func TestParseRequestValid(t *testing.T) {
	for _, amount := range []int{1, 100, 101, 10000, 10001} {
		line := "PAYMENT|" + strconv.Itoa(amount)
		got, err := parseRequest(line)
		if err != nil {
			t.Fatalf("parseRequest(%q) err = %v; want nil", line, err)
		}
		if got != amount {
			t.Errorf("parseRequest(%q) amount = %d; want %d", line, got, amount)
		}
	}
}

func TestRejectReason(t *testing.T) {
	if got := rejectReason(errInvalidRequest); got != "Invalid request" {
		t.Errorf("rejectReason(errInvalidRequest) = %q; want %q", got, "Invalid request")
	}
	if got := rejectReason(errInvalidAmount); got != "Invalid amount" {
		t.Errorf("rejectReason(errInvalidAmount) = %q; want %q", got, "Invalid amount")
	}
}

// A leading '+' is accepted: "+100" is a positive integer. See DECISIONS.md.
func TestParseRequestLeadingPlus(t *testing.T) {
	got, err := parseRequest("PAYMENT|+100")
	if err != nil {
		t.Fatalf(`parseRequest("PAYMENT|+100") err = %v; want nil`, err)
	}
	if got != 100 {
		t.Errorf(`parseRequest("PAYMENT|+100") amount = %d; want 100`, got)
	}
}

func TestParseRequestMalformed(t *testing.T) {
	cases := map[string]string{
		"wrong prefix":       "INVALID|100",
		"lowercase prefix":   "payment|100",
		"empty prefix":       "|100",
		"missing separator":  "PAYMENT",
		"empty line":         "",
		"extra field":        "PAYMENT|100|extra",
		"trailing separator": "PAYMENT|100|",
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseRequest(line)
			if !errors.Is(err, errInvalidRequest) {
				t.Errorf("parseRequest(%q) err = %v; want errInvalidRequest", line, err)
			}
		})
	}
}

func TestParseRequestInvalidAmount(t *testing.T) {
	cases := map[string]string{
		"zero":            "PAYMENT|0",
		"negative":        "PAYMENT|-5",
		"non-numeric":     "PAYMENT|abc",
		"decimal":         "PAYMENT|1.5",
		"leading space":   "PAYMENT| 100",
		"trailing space":  "PAYMENT|100 ",
		"empty amount":    "PAYMENT|",
		"overflow":        "PAYMENT|99999999999999999999",
		"carriage return": "PAYMENT|100\r", // parseRequest assumes framing already stripped
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseRequest(line)
			if !errors.Is(err, errInvalidAmount) {
				t.Errorf("parseRequest(%q) err = %v; want errInvalidAmount", line, err)
			}
		})
	}
}

const delayTolerance = 50 * time.Millisecond

func TestProcessingDelayNoDelayAtOrBelowThreshold(t *testing.T) {
	for _, amount := range []int{1, 50, 100} {
		start := time.Now()
		err := processingDelay(context.Background(), amount)
		elapsed := time.Since(start)

		if err != nil {
			t.Errorf("processingDelay(ctx, %d) = %v; want nil", amount, err)
		}
		if elapsed > delayTolerance {
			t.Errorf("processingDelay(ctx, %d) took %v; want ~0", amount, elapsed)
		}
	}
}

func TestProcessingDelayAboveThreshold(t *testing.T) {
	const amount = 200
	want := amount * time.Millisecond

	start := time.Now()
	err := processingDelay(context.Background(), amount)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("processingDelay(ctx, %d) = %v; want nil", amount, err)
	}
	if elapsed < want || elapsed > want+delayTolerance {
		t.Errorf("processingDelay(ctx, %d) took %v; want ~%v", amount, elapsed, want)
	}
}

func TestProcessingDelayCappedAt10s(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 10s delay test under -short")
	}
	const amount = 20000 // 20s uncapped
	want := 10 * time.Second

	start := time.Now()
	err := processingDelay(context.Background(), amount)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("processingDelay(ctx, %d) = %v; want nil", amount, err)
	}
	if elapsed < want || elapsed > want+delayTolerance {
		t.Errorf("processingDelay(ctx, %d) took %v; want ~%v (capped)", amount, elapsed, want)
	}
}

func TestProcessingDelayCancelledMidDelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	const amount = 5000
	start := time.Now()
	err := processingDelay(ctx, amount)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("processingDelay(cancelled ctx, %d) = %v; want context.Canceled", amount, err)
	}
	if elapsed > 20*time.Millisecond+delayTolerance {
		t.Errorf("processingDelay returned %v after cancel; want prompt return", elapsed)
	}
}

// A cancelled context wins regardless of amount: a draining server must not
// accept even a sub-threshold (zero-delay) request.
func TestProcessingDelayContextAlreadyCancelled(t *testing.T) {
	for _, amount := range []int{50, 5000} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		start := time.Now()
		err := processingDelay(ctx, amount)
		elapsed := time.Since(start)

		if !errors.Is(err, context.Canceled) {
			t.Errorf("processingDelay(cancelled ctx, %d) = %v; want context.Canceled", amount, err)
		}
		if elapsed > delayTolerance {
			t.Errorf("processingDelay(cancelled ctx, %d) took %v; want immediate return", amount, elapsed)
		}
	}
}

func TestNewServerDefaultGracePeriod(t *testing.T) {
	if got := NewServer("127.0.0.1:0", 0).gracePeriod; got != defaultGracePeriod {
		t.Errorf("gracePeriod = %v; want default %v", got, defaultGracePeriod)
	}
	if got := NewServer("127.0.0.1:0", 5*time.Second).gracePeriod; got != 5*time.Second {
		t.Errorf("gracePeriod = %v; want 5s", got)
	}
}

func TestServerAddrEmptyBeforeStart(t *testing.T) {
	if got := NewServer("127.0.0.1:0", time.Second).Addr(); got != "" {
		t.Errorf("Addr() before Start = %q; want empty", got)
	}
}

func TestServerStartBindsAndAccepts(t *testing.T) {
	s := NewServer("127.0.0.1:0", time.Second)
	if err := s.Start(); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	host, port, err := net.SplitHostPort(s.Addr())
	if err != nil {
		t.Fatalf("Addr() = %q: %v", s.Addr(), err)
	}
	if host != "127.0.0.1" {
		t.Errorf("Addr() host = %q; want 127.0.0.1", host)
	}
	if port == "" || port == "0" {
		t.Errorf("Addr() port = %q; want the OS-assigned port", port)
	}

	conn, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatalf("Dial(%q) = %v; want a listening server", s.Addr(), err)
	}
	_ = conn.Close()
}

func TestServerStartRejectsBusyAddr(t *testing.T) {
	s1 := NewServer("127.0.0.1:0", time.Second)
	if err := s1.Start(); err != nil {
		t.Fatalf("s1.Start() = %v", err)
	}
	t.Cleanup(func() { _ = s1.Shutdown(context.Background()) })

	s2 := NewServer(s1.Addr(), time.Second)
	if err := s2.Start(); err == nil {
		_ = s2.Shutdown(context.Background())
		t.Fatalf("s2.Start() on in-use %s = nil; want a bind error", s1.Addr())
	}
}

func TestServerShutdownStopsListener(t *testing.T) {
	s := NewServer("127.0.0.1:0", time.Second)
	if err := s.Start(); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	addr := s.Addr()

	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}

	if conn, err := net.Dial("tcp", addr); err == nil {
		_ = conn.Close()
		t.Fatal("Dial succeeded after Shutdown; want connection refused")
	}
}

func TestHandleRequest(t *testing.T) {
	cases := map[string]struct{ line, want string }{
		"valid":           {"PAYMENT|10", acceptedResponse},
		"valid at delay":  {"PAYMENT|101", acceptedResponse},
		"invalid request": {"NOPE", "RESPONSE|REJECTED|Invalid request"},
		"invalid amount":  {"PAYMENT|0", "RESPONSE|REJECTED|Invalid amount"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := handleRequest(context.Background(), tc.line); got != tc.want {
				t.Errorf("handleRequest(%q) = %q; want %q", tc.line, got, tc.want)
			}
		})
	}
}

// A context cancelled before the delay elapses turns an otherwise-valid request
// into RESPONSE|REJECTED|Cancelled. Step 5 wires the server side that triggers it.
func TestHandleRequestCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := handleRequest(ctx, "PAYMENT|5000"); got != "RESPONSE|REJECTED|Cancelled" {
		t.Errorf("handleRequest(cancelled ctx, PAYMENT|5000) = %q; want RESPONSE|REJECTED|Cancelled", got)
	}
}

func TestServeSequentialRequestsOnOneConnection(t *testing.T) {
	s := startTestServer(t, time.Second)
	conn := dial(t, s)
	r := bufio.NewReader(conn)

	steps := []struct{ req, want string }{
		{"PAYMENT|10", acceptedResponse},
		{"PAYMENT|-1", "RESPONSE|REJECTED|Invalid amount"},
		{"GARBAGE", "RESPONSE|REJECTED|Invalid request"},
		{"PAYMENT|50", acceptedResponse},
		{"PAYMENT|101", acceptedResponse},
	}
	for _, step := range steps {
		if got := request(t, conn, r, step.req); got != step.want {
			t.Errorf("%q -> %q; want %q", step.req, got, step.want)
		}
	}
}

// Two requests are written back-to-back without reading in between. The server
// must answer the slow one first and only then process the fast one — one
// request at a time, responses in order.
func TestServeOneRequestAtATime(t *testing.T) {
	s := startTestServer(t, time.Second)
	conn := dial(t, s)

	if _, err := io.WriteString(conn, "PAYMENT|250\nPAYMENT|10\n"); err != nil {
		t.Fatalf("write: %v", err)
	}

	r := bufio.NewReader(conn)
	start := time.Now()

	first, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read first: %v", err)
	}
	firstAt := time.Since(start)

	second, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read second: %v", err)
	}

	if firstAt < 250*time.Millisecond {
		t.Errorf("first response at %v; want >= 250ms (slow request processed before the fast one)", firstAt)
	}
	if got := strings.TrimRight(first, "\r\n"); got != acceptedResponse {
		t.Errorf("first response = %q; want %q", got, acceptedResponse)
	}
	if got := strings.TrimRight(second, "\r\n"); got != acceptedResponse {
		t.Errorf("second response = %q; want %q", got, acceptedResponse)
	}
}

func TestServeConcurrentConnections(t *testing.T) {
	s := startTestServer(t, time.Second)

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)

	start := time.Now()
	for i := range n {
		wg.Go(func() {
			conn, err := net.Dial("tcp", s.Addr())
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = conn.Close() }()

			if _, err := io.WriteString(conn, "PAYMENT|150\n"); err != nil {
				errs <- fmt.Errorf("conn %d write: %w", i, err)
				return
			}
			resp, err := bufio.NewReader(conn).ReadString('\n')
			if err != nil {
				errs <- fmt.Errorf("conn %d read: %w", i, err)
				return
			}
			if got := strings.TrimRight(resp, "\r\n"); got != acceptedResponse {
				errs <- fmt.Errorf("conn %d: got %q, want %q", i, got, acceptedResponse)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// 20 connections each delaying 150ms: concurrent handling finishes well
	// under the ~3s a serial server would take. Budget is loose to tolerate
	// scheduling jitter under -race on a loaded machine.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("20 concurrent connections took %v; want concurrent handling", elapsed)
	}
}

func TestServeCRLFFraming(t *testing.T) {
	s := startTestServer(t, time.Second)
	conn := dial(t, s)

	if _, err := io.WriteString(conn, "PAYMENT|10\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := strings.TrimRight(resp, "\r\n"); got != acceptedResponse {
		t.Errorf("CRLF-terminated request -> %q; want %q", got, acceptedResponse)
	}
}

// A request with no terminating newline is not a complete frame: when the client
// half-closes, the server discards it without responding.
func TestServeUnterminatedRequestGetsNoResponse(t *testing.T) {
	s := startTestServer(t, time.Second)
	conn := dial(t, s)

	if _, err := io.WriteString(conn, "PAYMENT|10"); err != nil {
		t.Fatalf("write: %v", err)
	}
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		t.Fatalf("conn is %T, want *net.TCPConn", conn)
	}
	if err := tcp.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	if resp, err := bufio.NewReader(conn).ReadString('\n'); err == nil {
		t.Errorf("got response %q; want none for an unterminated request", strings.TrimRight(resp, "\r\n"))
	}
}

// --- Step 5: graceful shutdown ---

// A request read off the socket just before shutdown completes normally as long
// as it finishes inside the grace period.
func TestShutdownServesInflightWithinGrace(t *testing.T) {
	s := startTestServer(t, 2*time.Second)
	conn := dial(t, s)

	if _, err := io.WriteString(conn, "PAYMENT|400\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // let the server read it and start processing

	start := time.Now()
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	elapsed := time.Since(start)

	resp, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if got := strings.TrimRight(resp, "\r\n"); got != acceptedResponse {
		t.Errorf("response = %q; want %q (finished within grace)", got, acceptedResponse)
	}
	if elapsed > time.Second {
		t.Errorf("Shutdown took %v; want ~350ms (drained well before the 2s grace)", elapsed)
	}
}

// A request still processing when the grace period expires is rejected with
// Cancelled — and that response is delivered, not just the socket closed.
func TestShutdownCancelsInflightExceedingGrace(t *testing.T) {
	s := startTestServer(t, 200*time.Millisecond)
	conn := dial(t, s)

	if _, err := io.WriteString(conn, "PAYMENT|5000\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	elapsed := time.Since(start)

	resp, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if got := strings.TrimRight(resp, "\r\n"); got != "RESPONSE|REJECTED|Cancelled" {
		t.Errorf("response = %q; want RESPONSE|REJECTED|Cancelled", got)
	}
	if elapsed > time.Second {
		t.Errorf("Shutdown took %v; want ~200ms grace, not the full 5s delay", elapsed)
	}
}

// An idle connection (blocked waiting for its next request) is not an in-flight
// request: shutdown drops it immediately instead of waiting out the grace period.
func TestShutdownDropsIdleConnectionPromptly(t *testing.T) {
	s := startTestServer(t, 5*time.Second)
	conn := dial(t, s)
	time.Sleep(50 * time.Millisecond) // ensure the serve goroutine is blocked in ReadString

	start := time.Now()
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Shutdown took %v with only an idle connection; want prompt", elapsed)
	}

	if _, err := bufio.NewReader(conn).ReadString('\n'); err == nil {
		t.Error("idle connection got a response; want none")
	}
}

// New connections are refused once Shutdown has closed the listener.
func TestShutdownRefusesNewConnections(t *testing.T) {
	s := startTestServer(t, time.Second)
	addr := s.Addr()

	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}

	if conn, err := net.Dial("tcp", addr); err == nil {
		_ = conn.Close()
		t.Fatal("Dial succeeded after Shutdown; want connection refused")
	}
}

// After a full start / serve / shutdown cycle, no goroutines are left behind —
// in particular the per-connection bridge goroutines.
func TestShutdownNoGoroutineLeak(t *testing.T) {
	base := runtime.NumGoroutine()

	s := startTestServer(t, time.Second)

	for i := range 5 {
		conn, err := net.Dial("tcp", s.Addr())
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		if _, err := io.WriteString(conn, "PAYMENT|10\n"); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		_ = conn.Close()
	}

	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}

	// Goroutines unwind asynchronously; poll back toward the baseline.
	deadline := time.Now().Add(2 * time.Second)
	for {
		n := runtime.NumGoroutine()
		if n <= base {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: %d running, baseline %d", n, base)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
