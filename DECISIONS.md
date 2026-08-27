# Decisions

## Step 1 — Parsing and validation

- **`parseRequest(line string) (int, error)`** with sentinel errors
  `errInvalidRequest` / `errInvalidAmount`. `error` is the idiomatic "can fail"
  signal — no `ok bool` to explain — and the two failure modes are named rather
  than bare string literals. Tests assert with `errors.Is`.

  *How we got here:* the first cut returned `(int, string, bool)`, where the
  string was the wire reason. That worked but read poorly — a reader had to
  learn that the string is only meaningful when the bool is false. Switching to
  `error` fixed that, but a second problem appeared: piping `err.Error()`
  straight to the wire forced the error strings to be `"Invalid request"` /
  `"Invalid amount"` — capitalised, which `golangci-lint` (ST1005) flags,
  because non-composable error text is a real smell. So the error values are now
  lowercase and idiomatic, and the wire strings live in their own `const` block
  (`reasonInvalidRequest`, `reasonInvalidAmount`). `rejectReason(err)` maps one
  to the other. This is one small mapping function; it is worth it to keep both
  the errors and the protocol tokens honest.

- **Check order: structure before amount.** `INVALID|abc` is `Invalid request`, not
  `Invalid amount` — the prefix/field-count check runs first.

- **`parseRequest` assumes framing is already stripped.** It operates on the exact
  string. Trailing `\r` (from a CRLF client) therefore fails as `Invalid amount`.
  Stripping `\r\n` is the read loop's job (Step 4).

- **The amount rule is exactly one sentence:** valid iff `strconv.Atoi` parses the
  field *as-is* and the result is `> 0`. Everything else follows from that single
  mechanism, consistently:
  - accepted: `100`, `+100` (Atoi takes a leading `+`)
  - rejected: `0`, `-5` (not positive); `abc`, `1.5`, `0x10` (not an integer);
    empty; 20-digit overflow; `PAYMENT| 100` / `PAYMENT|100 ` (Atoi does not trim).
  We deliberately do **not** pre-trim the field — the protocol specifies an exact
  format and tolerating padding would mask client bugs in a scheme simulator.

- **Standard library only.** Dropped the `testify` dependency the prototype used;
  tests use `testing` alone.

## Step 2 — Context-aware processing delay

- **`processingDelay(ctx context.Context, amount int) error`**, a free function.
  Returns `nil` once the delay elapses, `ctx.Err()` if the context is cancelled
  first. No server state involved, so it stays a plain testable function.

- **A cancelled context wins even for sub-threshold amounts.** `amount <= 100`
  has no delay, but `processingDelay` still checks `ctx.Err()` first and returns
  it. During shutdown a draining server should reject *every* new request once
  the grace period is up, not just the slow ones — and the caller shouldn't have
  to know which amounts are cancellable.

- **Cap: `min(amount ms, 10s)`.** `amount == 10000` is already exactly 10s, so a
  single `min` expresses "10s max for amounts over 10 000".

- **`time.NewTimer` + `select`, timer stopped on return.** No goroutine, no leak.

- **10s cap test runs in real time, skipped under `-short`.** Injecting a clock
  to make it fast would add an abstraction the rest of the code doesn't need.
  `make test` runs the full suite; `go test -short` is the fast inner loop.
  Time assertions use a 50ms tolerance, as the brief allows.

## Step 3 — Server type and lifecycle

- **`Start()` binds synchronously, accepts asynchronously.** Unlike
  `http.ListenAndServe`, `Start` returns as soon as the listener is bound, so
  bind errors surface as a return value and tests read `Addr()` immediately with
  no sleeping. The accept loop runs in its own goroutine.

- **`Addr()` returns a string snapshotted during `Start`, not `listener.Addr()`.**
  It is written once, before the accept goroutine exists, and every `Addr()` call
  is ordered after `Start()` returns — so it needs no mutex and stays `-race`
  clean. Contract: call `Start` (and check its error) before `Addr()`; calling
  `Addr()` concurrently with an in-progress `Start` is misuse. Bonus: the
  snapshot keeps working after `Shutdown` closes the listener.

- **The accept loop is tracked by a channel (`acceptDone`), connections by a
  `sync.WaitGroup` (`conns`).** A WaitGroup is a dynamic counter; "wait for this
  one goroutine" is a channel. Keeping them separate means `conns.Wait()` in
  `Shutdown` means exactly "every in-flight connection is done", and the Step 5
  grace-period deadline never accidentally applies to the accept loop.
  `acceptDone` is created in `NewServer` so a receive on it can never block
  forever.

- **Every `Accept` error is terminal.** The expected one at shutdown is
  `net.ErrClosed` (silent); anything else is logged and also ends the loop. The
  brief excludes advanced network conditions, so there is no retry/backoff on
  transient accept errors.

- **`Shutdown(ctx context.Context) error` is built incrementally.** Step 3: close
  the listener, wait for `acceptDone`, then wait for `conns` or `ctx`. Step 5
  adds the grace period → `RESPONSE|REJECTED|Cancelled` → close behaviour and
  makes it idempotent (see the Step 5 section). It is called after a successful
  `Start`.

- **Config is two constructor args, not a `Config` struct.** `addr` and
  `gracePeriod` don't warrant a struct yet; `NewServer` clamps a non-positive
  grace period to `defaultGracePeriod` (3s). `main` owns signal handling and
  drives `Shutdown` with a `defaultGracePeriod` timeout context.

- **`conns.Go(func(){...})`** (Go 1.25) instead of manual `Add`/`Done` — same
  semantics, less boilerplate. Safe to rely on: the repo's own `go.mod` has
  declared `go 1.26.4` since the initial commit, so the language floor is above
  1.25.

## Step 4 — Connection read loop

- **`serve` uses `bufio.Reader.ReadString('\n')`, not `bufio.Scanner`.**
  `ReadString` keeps the delimiter (so an unterminated final line is visibly
  incomplete and we return an error), has no hidden token-size limit to trip
  over, and pairs naturally with `io.WriteString(conn, resp+"\n")`.

- **One goroutine per connection, one request at a time within it.** The loop
  reads a full line, processes it to completion, writes the response, then reads
  again — so responses are strictly ordered and a slow request blocks later ones
  on the *same* connection (proven by `TestServeOneRequestAtATime`, which
  pipelines a slow + fast pair). Different connections run concurrently.

- **`strings.TrimRight(line, "\r\n")` strips framing before `parseRequest`.** A
  payload never legitimately ends in `\r` or `\n`; this is why `parseRequest`
  treats a trailing `\r` as invalid (Step 1) — by its contract, framing is
  already gone.

- **An unterminated request gets no response.** `ReadString` returns an error
  (no delimiter) and `serve` returns, discarding the partial line. This is the
  "requests not yet fully received may be dropped without a response" case.

- **No line-length cap.** A client that streams bytes without a newline grows
  the buffer unboundedly. The brief explicitly scopes out slow clients and
  adversarial network conditions, so a cap would defend against a threat model
  that isn't in play.

- **`handleRequest(ctx, line)` and its `Cancelled` branch exist now, wired in
  Step 5.** `serve` passes `context.Background()` this step, so the branch is
  unreachable from the server — but it is unit-tested directly
  (`TestHandleRequestCancelled`). The alternative, `_ = processingDelay(...)`,
  would silently drop cancellation and leave nothing to wire in Step 5. This
  also deletes the prototype's duplicate inline `time.Sleep` + cap.

## Step 5 — Graceful shutdown

- **Two phases, one ordering.** `Shutdown` does: close the listener → wait for
  `acceptDone` → `close(s.shutdown)` → start the grace timer → wait for `drained`
  or the caller's `ctx`. `close(s.shutdown)` *before* the timer is what lets idle
  connections leave in phase 1 instead of sitting through the grace period.

- **Idle vs in-flight is positional, enforced by two mechanisms.**
  `close(s.shutdown)` triggers a per-connection bridge goroutine that sets a past
  read deadline — waking anything blocked in `ReadString` (idle, "not accepted":
  it exits with no response). A connection between a completed read and its write
  is not reading, so the deadline is inert; it runs under the processing context
  until either it finishes or the grace timer fires `s.cancelProc()`, turning
  `processingDelay` into `RESPONSE|REJECTED|Cancelled`.

- **One bridge goroutine per connection.** It only translates a channel close
  into a `SetReadDeadline` call, then exits via `defer close(stop)` on every
  `serve` return path. A goroutine purely to bridge a signal to a deadline is a
  deliberate trade — the alternative, an `http.Server`-style mutex-guarded
  connection map, is more shared state than this earns. `TestShutdownNoGoroutineLeak`
  guards against the bridge outliving its connection.

- **The processing context is a parameter, never a struct field.** `Start`
  creates it and threads it `acceptLoop(ctx)` → `serve(ctx, conn)`; only the
  `CancelFunc` is stored. Avoids holding a `context.Context` in a struct.

- **No write deadline.** A peer that never reads its response would hold one
  `serve` goroutine and delay `conns.Wait()` past the grace period. The brief
  excludes slow/stuck clients, so this is unhandled by design. `main` passes a
  `gracePeriod + 2s` backstop context to `Shutdown` so the *process* still exits
  in bounded time even then; `Shutdown` itself owns the grace timing via
  `s.gracePeriod`, and its `ctx` argument is only that backstop.

- **`Shutdown` is idempotent** (`sync.Once`): tests call it both explicitly and
  via `t.Cleanup`, and a shutdown method that is safe to call twice is good
  hygiene regardless. Second and later calls just wait on `drained`.

- **Drained-before-grace is fine.** If everything finishes quickly, `drained`
  closes, the timer goroutine's `select` takes that branch and calls
  `s.cancelProc()` on a context nobody is using — harmless — and `Shutdown`
  returns `nil` from its own `select` independently.
