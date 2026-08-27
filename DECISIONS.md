# Decisions

## Assumptions

Where the brief left room for interpretation, these are the choices made and why.

- **Valid amount = `strconv.Atoi` parses the field as-is and the result is `> 0`.**
  One rule covers every case: `100` and `+100` are accepted (`Atoi` takes a
  leading `+`); `0`, `-5`, `abc`, `1.5`, the empty string, a 20-digit overflow,
  and an amount field with a leading or trailing space are all `Invalid amount`.
  The field is **not** trimmed — the protocol specifies an exact format, and
  tolerating padding would mask client bugs in a simulator.

- **Structure is validated before the amount.** `INVALID|abc` is `Invalid
  request`, not `Invalid amount`.

- **The accepted response keeps the prototype's reason string,
  `Transaction processed`.** The spec only says the reason is "additional
  details if accepted", so rather than invent wording this preserves the
  existing wire contract.

- **Both `\n` and `\r\n` terminate a message.** The read loop strips a trailing
  `\r`/`\n`; a payload never legitimately ends that way.

- **A line with no terminator is not a request.** If a connection ends
  mid-line, the partial data is discarded with no response — the "not yet
  received" case from the brief.

- **The delay cap is `min(amount ms, 10s)`.** `amount == 10000` is already
  exactly 10s, so "10s max for amounts over 10 000" needs no special case.

- **Shutdown rejects every new request once the grace period expires**,
  including amounts ≤ 100 that have no delay. The client should not have to know
  which amounts are cancellable.

- **Grace period: 3s default, set via `NewServer`.** A non-positive value falls
  back to the default.

- **Out of scope, per the brief's exclusions:** no cap on line length (a client
  streaming bytes without a newline can grow the read buffer), and no write
  deadline (a peer that never reads its response holds one goroutine). `main`
  passes a `grace + 2s` backstop context so the *process* still exits promptly
  in that last case.

- **Standard library only.** The prototype's `testify` dependency was removed.

## Design

### Request handling

- **`parseRequest(line) (int, error)`** returns the amount or one of two
  sentinel errors. `error` is the idiomatic "can fail" signal. The sentinels are
  lowercase (`golangci-lint` ST1005 — error text should compose); the
  wire-format strings live in their own `const` block, and `rejectReason(err)`
  maps between them. One small function keeps both the errors and the protocol
  tokens honest.

- **`processingDelay(ctx, amount) error`** is a free function — no server state.
  `time.NewTimer` + `select` on the timer and `ctx.Done()`, timer stopped on
  return, so there is no leak and no extra goroutine.

- **`handleRequest(ctx, line) string`** is the one place that ties
  validate → delay → respond together, so `serve` stays a plain I/O loop.

### Server

- **`NewServer(addr, gracePeriod)`** — two constructor arguments, not a `Config`
  struct; that is all the configuration there is.

- **`Start` binds synchronously, serves asynchronously.** It returns as soon as
  the listener is bound, so bind errors are a return value and tests read
  `Addr()` without sleeping. `Addr()` returns a string snapshotted during
  `Start` — written once before the accept goroutine exists, always read after
  `Start` returns, so no mutex — and it keeps working after `Shutdown`.

- **Accept loop tracked by a channel (`acceptDone`), connections by a
  `sync.WaitGroup` (`conns`).** Different primitives for different jobs: "wait
  for one goroutine" is a channel, "wait for a dynamic set" is a WaitGroup.
  `conns.Wait()` then means exactly "every connection has ended".

- **Every `Accept` error ends the loop.** `net.ErrClosed` at shutdown is
  expected and silent; anything else is logged. No retry/backoff — transient
  accept errors are out of scope.

- **`serve` reads with `bufio.Reader.ReadString('\n')`**, one request at a time:
  read a full line, handle it to completion, write the response, read again.
  Responses are strictly ordered and a slow request blocks later ones on the
  same connection; different connections run concurrently.

### Graceful shutdown

- **Two phases, fixed order:** close the listener → wait for `acceptDone` →
  `close(s.shutdown)` → start the grace timer → wait for `drained` or the
  caller's `ctx`. Closing `s.shutdown` before the timer is what lets idle
  connections leave immediately rather than sitting through the grace period.

- **Idle vs in-flight is positional.** `close(s.shutdown)` fires a
  per-connection bridge goroutine that sets a past read deadline: a connection
  blocked in `ReadString` (idle) wakes and exits with no response. A connection
  between a completed read and its write is not reading, so the deadline is
  inert — it runs under the processing context until it finishes or the grace
  timer calls `cancelProc()`, which turns `processingDelay` into
  `RESPONSE|REJECTED|Cancelled` (still written before the connection closes;
  read and write deadlines are independent).

- **One bridge goroutine per connection**, cleaned up by `defer close(stop)` on
  every `serve` return path. Bridging a channel close to a `SetReadDeadline`
  call is a deliberate trade against an `http.Server`-style mutex-guarded
  connection map — less shared state for this size. `TestShutdownNoGoroutineLeak`
  guards it.

- **The processing context is threaded `Start → acceptLoop → serve` as a
  parameter**; only its `CancelFunc` is stored on the struct.

- **`Shutdown` is idempotent** (`sync.Once`) — tests call it both directly and
  via `t.Cleanup`, and a shutdown that is safe to call twice is good hygiene.
  It owns the grace timing via `s.gracePeriod`; its `ctx` argument is only the
  process backstop.

### Testing

- **The 10s delay-cap test runs in real time and is skipped under `-short`.**
  A clock abstraction to make it fast would not earn its place. Time assertions
  use a 50ms tolerance, as the brief allows.

- **The leak test polls `runtime.NumGoroutine()` back toward a baseline** rather
  than sampling once — goroutines unwind asynchronously.

- **`WaitGroup.Go` (Go 1.25)** is used instead of manual `Add`/`Done`; the
  repo's `go.mod` has declared `go 1.26.4` since the initial commit.
