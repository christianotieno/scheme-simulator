package main

import (
	"bufio"
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

func handleRequest(request string) string {
	parts := strings.Split(request, "|")
	if len(parts) != 2 || parts[0] != "PAYMENT" {
		return "RESPONSE|REJECTED|Invalid request"
	}

	amount, err := strconv.Atoi(parts[1])
	if err != nil {
		return "RESPONSE|REJECTED|Invalid amount"
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
