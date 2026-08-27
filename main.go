package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// defaultGracePeriod is how long in-flight requests may keep running once
// shutdown has begun before they are cancelled.
const defaultGracePeriod = 3 * time.Second

// Server is a TCP scheme simulator. Construct it with NewServer, then Start;
// call Shutdown once to stop it.
type Server struct {
	addr        string
	gracePeriod time.Duration

	listener   net.Listener
	boundAddr  string
	conns      sync.WaitGroup // one per in-flight connection; drained by Shutdown
	acceptDone chan struct{}  // closed when the accept loop returns

	shutdown     chan struct{}      // closed by Shutdown: serve loops stop reading new requests
	drained      chan struct{}      // closed when every connection goroutine has returned
	cancelProc   context.CancelFunc // cancels request processing when the grace period expires
	shutdownOnce sync.Once
}

// NewServer returns a Server that will bind to addr. A non-positive gracePeriod
// is replaced with defaultGracePeriod.
func NewServer(addr string, gracePeriod time.Duration) *Server {
	if gracePeriod <= 0 {
		gracePeriod = defaultGracePeriod
	}
	return &Server{
		addr:        addr,
		gracePeriod: gracePeriod,
		acceptDone:  make(chan struct{}),
		shutdown:    make(chan struct{}),
		drained:     make(chan struct{}),
	}
}

// Start binds the listener and runs the accept loop in the background. Bind
// errors are returned synchronously; once Start returns nil the server is
// accepting and Addr reports the resolved address.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.listener = ln
	s.boundAddr = ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	s.cancelProc = cancel

	go s.acceptLoop(ctx)
	return nil
}

// Addr is the resolved listen address, or "" before a successful Start.
func (s *Server) Addr() string {
	return s.boundAddr
}

func (s *Server) acceptLoop(ctx context.Context) {
	defer close(s.acceptDone)
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			// Every accept error is treated as terminal (see DECISIONS.md); the
			// expected one during shutdown is net.ErrClosed.
			if !errors.Is(err, net.ErrClosed) {
				log.Printf("accept: %v", err)
			}
			return
		}
		s.conns.Go(func() { s.serve(ctx, conn) })
	}
}

// serve reads newline-terminated requests from one connection and writes one
// response per request, strictly in order. It returns when the client closes
// the connection, a read/write fails, or shutdown wakes a blocked read. An
// incomplete final line (no newline) is discarded without a response.
func (s *Server) serve(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()

	// Bridge shutdown to this connection: once Shutdown closes s.shutdown, a
	// past read deadline unblocks any in-progress read so an idle connection
	// exits immediately instead of waiting out the grace period. A connection
	// mid-request is unaffected — it is not reading — and finishes under ctx.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-s.shutdown:
			_ = conn.SetReadDeadline(time.Now())
		case <-stop:
		}
	}()

	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		resp := handleRequest(ctx, strings.TrimRight(line, "\r\n"))
		if _, err := io.WriteString(conn, resp+"\n"); err != nil {
			return
		}
	}
}

// Shutdown stops accepting connections, then drains in-flight requests: they get
// the grace period to finish normally, after which any still running are
// cancelled (yielding RESPONSE|REJECTED|Cancelled) and their connections closed.
// Connections idle between requests are dropped at once, without a response.
// Shutdown blocks until every connection has ended or ctx is cancelled; it is
// idempotent. Call after a successful Start.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownOnce.Do(func() {
		_ = s.listener.Close()
		<-s.acceptDone    // no new connection goroutines after this
		close(s.shutdown) // phase 1: wake idle reads — they exit now, not after grace

		go func() {
			s.conns.Wait()
			close(s.drained)
		}()
		go func() {
			timer := time.NewTimer(s.gracePeriod)
			defer timer.Stop()
			select {
			case <-timer.C: // phase 2: grace expired, cancel whatever is still running
			case <-s.drained: // everything finished in time
			}
			s.cancelProc()
		}()
	})

	select {
	case <-s.drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Wire-format rejection reasons.
const (
	reasonInvalidRequest = "Invalid request"
	reasonInvalidAmount  = "Invalid amount"
	reasonCancelled      = "Cancelled"
)

var (
	errInvalidRequest = errors.New("invalid request")
	errInvalidAmount  = errors.New("invalid amount")
)

// parseRequest parses a request line of the form "PAYMENT|<amount>" (framing
// already stripped), returning the amount or errInvalidRequest / errInvalidAmount.
func parseRequest(line string) (int, error) {
	rest, ok := strings.CutPrefix(line, "PAYMENT|")
	if !ok || strings.Contains(rest, "|") {
		return 0, errInvalidRequest
	}

	amount, err := strconv.Atoi(rest)
	if err != nil || amount <= 0 {
		return 0, errInvalidAmount
	}
	return amount, nil
}

// rejectReason maps a parseRequest error to its wire-format reason.
func rejectReason(err error) string {
	if errors.Is(err, errInvalidAmount) {
		return reasonInvalidAmount
	}
	return reasonInvalidRequest
}

const maxProcessingDelay = 10 * time.Second

// processingDelay blocks for the simulated counterparty processing delay: amount
// milliseconds for amounts over 100, capped at maxProcessingDelay, nothing at or
// below 100. It returns ctx.Err() if ctx is cancelled before the delay elapses.
func processingDelay(ctx context.Context, amount int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if amount <= 100 {
		return nil
	}

	timer := time.NewTimer(min(time.Duration(amount)*time.Millisecond, maxProcessingDelay))
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// handleRequest validates one request line, applies the processing delay, and
// returns the wire response. A ctx cancelled before the delay elapses yields
// RESPONSE|REJECTED|Cancelled.
func handleRequest(ctx context.Context, line string) string {
	amount, err := parseRequest(line)
	if err != nil {
		return "RESPONSE|REJECTED|" + rejectReason(err)
	}
	if err := processingDelay(ctx, amount); err != nil {
		return "RESPONSE|REJECTED|" + reasonCancelled
	}
	return "RESPONSE|ACCEPTED|Transaction processed"
}

const defaultAddr = "127.0.0.1:8080"

func main() {
	srv := NewServer(defaultAddr, defaultGracePeriod)
	if err := srv.Start(); err != nil {
		log.Fatalf("start: %v", err)
	}
	log.Printf("listening on %s", srv.Addr())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Printf("shutdown: draining connections (%s grace)", defaultGracePeriod)

	// Shutdown owns the grace-period timing; this context is only a backstop so a
	// wedged connection (e.g. a peer that never reads its response) can't hold
	// the process open past a bounded margin.
	ctx, cancel := context.WithTimeout(context.Background(), defaultGracePeriod+2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
