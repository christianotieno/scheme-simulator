package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func Start() error {
	listener, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", 8080))
	if err != nil {
		return err
	}
	defer listener.Close()

	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Println("Error accepting connection:", err)
			continue
		}

		go handleConnection(conn)
	}
}

func handleConnection(conn net.Conn) {
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		request := scanner.Text()
		response := handleRequest(request)
		fmt.Fprintf(conn, "%s\n", response)
	}

	if err := scanner.Err(); err != nil {
		fmt.Println("Error reading from connection:", err)
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

func main() {
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)

	go Start()

	<-shutdown
}
