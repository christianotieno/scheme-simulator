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
