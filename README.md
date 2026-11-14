# lumber-go

Go client for [Lumber](https://lumber.infinitetoken.com): cloud logging with end-to-end encryption. Every log is encrypted with AES-256-GCM before it leaves your process, so Lumber's servers store only ciphertext plus each log's level (kept readable for filtering and push alerts). You read your logs in the Lumber app, which decrypts them on your device.

No dependencies beyond the standard library. Logs are sent from a background goroutine, so logging never waits on the network, and it plugs into `log/slog`.

## Installation

```bash
go get github.com/infinitetoken/lumber-go
```

Go 1.22 or later.

## Quick start

```go
import lumber "github.com/infinitetoken/lumber-go"

client, err := lumber.New(os.Getenv("LUMBER_API_KEY"))
if err != nil {
	log.Fatal(err)
}
defer client.Close(context.Background()) // sends what's still queued

client.Info("Server started", "port", 8080)
client.Warn("Rate limit approaching", "request_id", "req-123", "user_id", 456)
client.Error("Payment failed", "trace_id", "trace-abc", "order_id", 789, "err", err)
```

The arguments after the message are `slog`'s: alternating keys and values, or `slog.Attr`. Call `Close` before your program exits, or the logs still queued are lost; give its context a deadline if exiting must not wait long.

## Getting your API key

Create an app in the Lumber app and it takes you straight to adding an API key. The key is shown only once, so copy it right away. Any device holding the app's key can add more from the app's **Secrets** screen, one per deployment if you like.

An API key is two values joined by a dot: a token, then the app's encryption key. The client sends only the token to Lumber and encrypts every log with the rest, so the encryption key never leaves your process. Keep the API key in an environment variable or your secrets manager, never in source control.

## With `log/slog`

`client.Handler` is a `slog.Handler`, so code that already logs with `slog` sends to Lumber:

```go
slog.SetDefault(slog.New(client.Handler(&lumber.HandlerOptions{Level: slog.LevelInfo, AddSource: true})))

slog.Info("Report ready", "request_id", "req-123", "rows", 1500, "took", 2400*time.Millisecond)

logger := slog.Default().With("service", "billing").WithGroup("job")
logger.Error("Job failed", "id", 42, "err", err)

logger.Log(ctx, lumber.LevelFatal, "Database unreachable")
```

Each record becomes a log at the matching level: from `slog.LevelError` up is ERROR, and from `lumber.LevelFatal` (12) up is FATAL. Attributes go into the log's metadata, with groups as nested objects, except three at the top level: `request_id` and `trace_id` (or `requestId` and `traceId`) are the log's own fields, and a `context` group or map goes into its context. An error is sent as its message. `AddSource` sends the file, line and function that logged in the context. `Level` defaults to `slog.LevelInfo`.

## Log methods

| Method | Level |
|--------|-------|
| `Debug(msg, args...)` | DEBUG |
| `Info(msg, args...)` | INFO |
| `Warn(msg, args...)` | WARN |
| `Error(msg, args...)` | ERROR |
| `Fatal(msg, args...)` | FATAL. It doesn't exit, unlike `log.Fatal` |
| `Flush(ctx)` | Waits for queued logs to send, or for `ctx` |
| `Close(ctx)` | Sends what's queued, waiting for `ctx` at most, and stops |

## Options

```go
client, err := lumber.New(os.Getenv("LUMBER_API_KEY"),
	lumber.WithEnvironment("production"), // sent as `env` in every log's context
	lumber.WithCaller(0),                 // send the file, line and function of the client's own calls; 1 skips a wrapper
	lumber.WithoutAutoContext(),          // don't send the process id and hostname
	lumber.WithTimeout(10*time.Second),   // how long to wait for Lumber to answer one log
	lumber.WithMaxQueue(10_000),          // logs waiting to send before new ones are dropped
	lumber.WithHTTPClient(httpClient),    // your own http.Client, for a proxy or custom TLS
	lumber.WithWarnings(func(msg string) { /* your logger */ }),
)
```

## Delivery

Logs are sent in order, one `POST /logs` per log, over a connection kept open between logs. A network error or a 5xx counts as a failed attempt: each log gets 5, waiting 1, 2, 4, then 8 seconds between them, and is then dropped with a warning. A log Lumber refuses (a 4xx, such as an invalid API key, or a 413 for a log over 16 KB once encrypted) is dropped at once with a warning, since sending it again won't help. Errors always get through: past your team's monthly logs, Lumber still takes error and fatal logs, and answers the rest 200 without keeping them, so the client carries on with no warning (the Lumber app shows when your team is at its limit). At twice your team's monthly logs Lumber answers 429: the client warns once and drops logs until the month turns. While Lumber can't be reached, up to `WithMaxQueue` logs wait; past that, new ones are dropped. Warnings go to standard error unless you pass `WithWarnings`.

## Development

```bash
go test -race ./...
```

`testdata/contract` holds [Lumber-Kit](https://github.com/infinitetoken/lumber-kit)'s contract at the version in `testdata/contract/VERSION`: a JSON Schema of the wire format and known answers for API keys, encryption, payloads and delivery, which Lumber's Node and Python clients are tested against too. `scripts/sync-contract.sh 0.8.0` moves it to a new kit version.

## License

MIT. See `LICENSE`.
