package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
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

	go s.acceptLoop()
	return nil
}

// Addr is the resolved listen address, or "" before a successful Start.
func (s *Server) Addr() string {
	return s.boundAddr
}

func (s *Server) acceptLoop() {
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
		s.conns.Go(func() { s.serve(conn) })
	}
}

// serve handles requests on one connection until the client disconnects.
func (s *Server) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		if _, err := fmt.Fprintf(conn, "%s\n", handleRequest(scanner.Text())); err != nil {
			return
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("connection read: %v", err)
	}
}

// Shutdown stops the listener and waits for in-flight connections to finish or
// for ctx to be cancelled. Call once, after a successful Start.
func (s *Server) Shutdown(ctx context.Context) error {
	_ = s.listener.Close()
	<-s.acceptDone

	done := make(chan struct{})
	go func() {
		s.conns.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Wire-format rejection reasons.
const (
	reasonInvalidRequest = "Invalid request"
	reasonInvalidAmount  = "Invalid amount"
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

func handleRequest(request string) string {
	amount, err := parseRequest(request)
	if err != nil {
		return "RESPONSE|REJECTED|" + rejectReason(err)
	}

	if amount > 100 {
		processingTime := amount
		if amount > 10000 {
			processingTime = 10000
		}
		time.Sleep(time.Duration(processingTime) * time.Millisecond)
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

	log.Println("shutdown: draining connections")
	ctx, cancel := context.WithTimeout(context.Background(), defaultGracePeriod)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
