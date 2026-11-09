// Package lumber sends logs to Lumber, cloud logging with end-to-end encryption. Every log is encrypted with
// AES-256-GCM before it leaves the process, so Lumber's servers store only ciphertext plus each log's level.
//
//	client, err := lumber.New(os.Getenv("LUMBER_API_KEY"))
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer client.Close(context.Background())
//
//	client.Info("Server started", "port", 8080)
//	slog.SetDefault(slog.New(client.Handler(nil)))
//
// Logging never blocks: logs are queued and sent from a background goroutine. Close sends what's queued before the
// program exits.
package lumber

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

// Version is this package's version, sent in the User-Agent.
const Version = "1.0.0"

// Client encrypts logs with an app's key and sends them to Lumber. It's safe for concurrent use.
type Client struct {
	aead        cipher.AEAD
	sender      *sender
	direct      *handler
	autoContext map[string]any
	caller      bool
	callerDepth int
	warn        func(string)
}

type options struct {
	endpoint    string
	environment string
	autoContext bool
	caller      bool
	callerDepth int
	httpClient  *http.Client
	timeout     time.Duration
	maxQueue    int
	warn        func(string)
}

// Option configures New.
type Option func(*options)

// WithEndpoint sends logs to another Lumber API, for testing against another server.
func WithEndpoint(url string) Option { return func(o *options) { o.endpoint = url } }

// WithEnvironment sends env (production, staging) with every log, as `env` in its context.
func WithEnvironment(env string) Option { return func(o *options) { o.environment = env } }

// WithoutAutoContext stops sending the process id and hostname with every log.
func WithoutAutoContext() Option { return func(o *options) { o.autoContext = false } }

// WithCaller sends the file, line and function that logged with each of the client's own log calls, skipping depth
// more frames, one for each wrapper around the client. A Handler sends them with HandlerOptions.AddSource instead.
func WithCaller(depth int) Option {
	return func(o *options) {
		o.caller = true
		o.callerDepth = depth
	}
}

// WithHTTPClient sends logs with your own http.Client (a proxy, custom TLS). Its Timeout applies, not WithTimeout's.
func WithHTTPClient(client *http.Client) Option { return func(o *options) { o.httpClient = client } }

// WithTimeout is how long to wait for Lumber to answer one log (10 seconds unless set).
func WithTimeout(timeout time.Duration) Option { return func(o *options) { o.timeout = timeout } }

// WithMaxQueue is how many logs can wait to send, while Lumber can't be reached, before new ones are dropped (10,000
// unless set).
func WithMaxQueue(n int) Option { return func(o *options) { o.maxQueue = n } }

// WithWarnings receives the client's warnings (a log refused, Lumber unreachable) in place of the standard error
// output. Pass a function that does nothing to silence them.
func WithWarnings(warn func(message string)) Option { return func(o *options) { o.warn = warn } }

// New makes a client from an app's API key, `<token>.<encryptionKey>`, from the app's Secrets screen in Lumber. Only the
// token is sent to Lumber; the encryption key encrypts every log and never leaves this process.
func New(apiKey string, opts ...Option) (*Client, error) {
	o := options{endpoint: DefaultEndpoint, autoContext: true, timeout: 10 * time.Second, maxQueue: 10_000}
	for _, opt := range opts {
		opt(&o)
	}

	token, encryptionKey, _ := splitAPIKey(apiKey)
	if token == "" {
		return nil, errors.New("lumber: missing API key. Create one on your app's Secrets screen in Lumber")
	}
	if encryptionKey == "" {
		return nil, errors.New("lumber: the API key has no encryption key. Copy a new API key from your app's Secrets screen in Lumber, which includes it")
	}
	key, err := base64.StdEncoding.Strict().DecodeString(encryptionKey)
	if err != nil || len(key) != keyBytes {
		return nil, fmt.Errorf("lumber: the API key's encryption key should be %d bytes of base64, got %d. Copy the API key again from your app's Secrets screen in Lumber", keyBytes, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(o.endpoint, "/")
	if !strings.HasPrefix(endpoint, "https://") && !strings.HasPrefix(endpoint, "http://") {
		return nil, fmt.Errorf("lumber: the endpoint must be an http or https URL, got %q", o.endpoint)
	}

	warn := o.warn
	if warn == nil {
		logger := log.New(os.Stderr, "[lumber] ", 0)
		warn = func(message string) { logger.Print(message) }
	}
	httpClient := o.httpClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: o.timeout}
	}
	userAgent := fmt.Sprintf("lumber-go/%s %s", Version, runtime.Version())

	c := &Client{
		aead:        aead,
		sender:      newSender(httpClient, endpoint, token, userAgent, max(1, o.maxQueue), warn),
		caller:      o.caller,
		callerDepth: o.callerDepth,
		warn:        warn,
	}
	if o.autoContext {
		c.autoContext = map[string]any{"pid": os.Getpid()}
		if hostname, err := os.Hostname(); err == nil && hostname != "" {
			c.autoContext["hostname"] = hostname
		}
		if o.environment != "" {
			c.autoContext["env"] = o.environment
		}
	}
	c.direct = &handler{client: c, level: slog.Level(-1 << 31), addSource: o.caller}
	return c, nil
}

// Debug logs at DEBUG. Its arguments are slog's: alternating keys and values, or slog.Attr. See Handler for the keys
// that mean something to Lumber.
func (c *Client) Debug(msg string, args ...any) { c.log(slog.LevelDebug, msg, args) }

// Info logs at INFO. See Debug for its arguments.
func (c *Client) Info(msg string, args ...any) { c.log(slog.LevelInfo, msg, args) }

// Warn logs at WARN. See Debug for its arguments.
func (c *Client) Warn(msg string, args ...any) { c.log(slog.LevelWarn, msg, args) }

// Error logs at ERROR. See Debug for its arguments.
func (c *Client) Error(msg string, args ...any) { c.log(slog.LevelError, msg, args) }

// Fatal logs at FATAL. Unlike log.Fatal it doesn't exit: call Close first if the program is about to.
func (c *Client) Fatal(msg string, args ...any) { c.log(LevelFatal, msg, args) }

// Flush waits until every queued log is sent (or dropped), or ctx is done.
func (c *Client) Flush(ctx context.Context) error { return c.sender.flush(ctx) }

// Close sends what's queued, waiting until ctx is done at most, and stops. Logs after this are dropped.
func (c *Client) Close(ctx context.Context) error { return c.sender.close(ctx) }

func (c *Client) log(level slog.Level, msg string, args []any) {
	var pc uintptr
	if c.caller {
		// The caller is three frames up: runtime.Callers, this method, then Debug, Info and the rest
		var pcs [1]uintptr
		runtime.Callers(3+c.callerDepth, pcs[:])
		pc = pcs[0]
	}
	record := slog.NewRecord(time.Now(), level, msg, pc)
	record.Add(args...)
	_ = c.direct.Handle(context.Background(), record)
}

// send builds, encrypts and queues one log. data takes Go's names (request_id, trace_id) or the API's; context goes
// over the automatic context, under the log's own.
func (c *Client) send(level int, message string, data, context map[string]any) {
	for name, wire := range map[string]string{"request_id": "requestId", "trace_id": "traceId"} {
		if value, ok := data[name]; ok {
			delete(data, name)
			if _, taken := data[wire]; !taken && value != nil {
				data[wire] = value
			}
		}
	}
	auto := c.autoContext
	if len(context) > 0 {
		auto = merge(c.autoContext, context)
	}
	encrypted, err := encrypt(c.aead, buildPayload(level, message, data, auto))
	if err != nil {
		c.warn(fmt.Sprintf("couldn't log %q: %v", message, err))
		return
	}
	body, err := toJSON(ingestBody{Level: level, Encrypted: encrypted})
	if err != nil {
		c.warn(fmt.Sprintf("couldn't log %q: %v", message, err))
		return
	}
	c.sender.enqueue(body)
}
