# Scheme Simulator

A TCP server that simulates a payment scheme for functional and non-functional
testing. It accepts `PAYMENT|<amount>` requests, delays the response in
proportion to the amount, and shuts down gracefully so in-flight requests can
finish.

## Protocol

Newline-terminated messages over TCP. Each connection handles one request at a
time and is reused for further requests.

```text
Request   PAYMENT|<amount>              <amount> is a positive integer
Response  RESPONSE|ACCEPTED|Transaction processed
          RESPONSE|REJECTED|Invalid request     malformed message
          RESPONSE|REJECTED|Invalid amount      amount not a positive integer
          RESPONSE|REJECTED|Cancelled           still running when the shutdown grace period expired
```

- Amounts over 100 delay the response by `<amount>` milliseconds, capped at 10s.
  Amounts of 100 or less are answered immediately.
- On shutdown the listener closes; in-flight requests get a grace period
  (default 3s) to finish, and any still running are answered
  `RESPONSE|REJECTED|Cancelled`. Connections idle between requests are dropped
  without a response.

## Build, run, test

Requires Go 1.26.4 or newer (the version declared in `go.mod`; `sync.WaitGroup.Go`
needs at least Go 1.25).

```sh
make build   # -> build/scheme-simulator
make run     # listens on 127.0.0.1:8080
make test    # go test -race ./...
```

```sh
$ printf 'PAYMENT|50\n' | nc 127.0.0.1 8080
RESPONSE|ACCEPTED|Transaction processed
```

`go test -short` skips the single real-time test (the 10-second delay cap).

## Design notes

[DECISIONS.md](DECISIONS.md) records the assumptions made about the requirements
and the rationale behind the design.
