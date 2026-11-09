package lumber

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// sender sends logs to POST /logs from one goroutine, in the order they were logged, over net/http's kept-alive
// connections. What each answer means and how long to wait are Lumber-Kit's (see contract.go).
type sender struct {
	http      *http.Client
	url       string
	headers   http.Header
	warn      func(string)
	retryWait func(failedAttempts int) time.Duration

	queue       chan []byte
	pending     atomic.Int64
	pausedUntil atomic.Int64 // Unix nanoseconds
	fullWarned  atomic.Bool
	closed      atomic.Bool
	stop        chan struct{}
	done        chan struct{}
	closeOnce   sync.Once
}

func newSender(httpClient *http.Client, endpoint, token, userAgent string, maxQueue int, warn func(string)) *sender {
	s := &sender{
		http:      httpClient,
		url:       endpoint + ingestPath,
		headers:   http.Header{"Authorization": {"Bearer " + token}, "Content-Type": {"application/json"}, "User-Agent": {userAgent}},
		warn:      warn,
		retryWait: retryDelay,
		queue:     make(chan []byte, maxQueue),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	go s.run()
	return s
}

// enqueue never blocks: a log that doesn't fit is dropped, with one warning until the queue has room again.
func (s *sender) enqueue(body []byte) {
	if s.closed.Load() || time.Now().UnixNano() < s.pausedUntil.Load() {
		return
	}
	s.pending.Add(1)
	select {
	case s.queue <- body:
		s.fullWarned.Store(false)
	default:
		s.pending.Add(-1)
		if !s.fullWarned.Swap(true) {
			s.warn(fmt.Sprintf("dropping logs: %d are already waiting to send", cap(s.queue)))
		}
	}
}

// flush waits until every queued log is sent or dropped, or ctx is done.
func (s *sender) flush(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for s.pending.Load() > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return nil
}

func (s *sender) close(ctx context.Context) error {
	err := s.flush(ctx)
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		close(s.stop)
	})
	select {
	case <-s.done:
	case <-ctx.Done():
		if err == nil {
			err = ctx.Err()
		}
	}
	s.http.CloseIdleConnections()
	return err
}

func (s *sender) run() {
	defer close(s.done)
	for {
		select {
		case <-s.stop:
			return
		case body := <-s.queue:
			s.deliver(body)
			s.pending.Add(-1)
		}
	}
}

func (s *sender) deliver(body []byte) {
	for failed := 0; ; {
		if time.Now().UnixNano() < s.pausedUntil.Load() {
			return
		}
		status, retryAfter, message, err := s.post(body)
		switch deliveryOutcome(status) {
		case outcomeSent:
			return
		case outcomePause:
			s.pause(retryAfter, message)
			return
		case outcomeDrop:
			// A bad key or a malformed log won't go through on a later try
			if message != "" {
				s.warn(fmt.Sprintf("log refused: HTTP %d (%s)", status, message))
			} else {
				s.warn(fmt.Sprintf("log refused: HTTP %d", status))
			}
			return
		}
		if err != nil {
			s.warn(fmt.Sprintf("send error: %v", err))
		} else {
			s.warn(fmt.Sprintf("send failed: HTTP %d", status))
		}
		failed++
		if failed >= maxAttempts {
			s.warn(fmt.Sprintf("dropping log after %d failed attempts", maxAttempts))
			return
		}
		timer := time.NewTimer(s.retryWait(failed))
		select {
		case <-timer.C:
		case <-s.stop:
			timer.Stop()
			return
		}
	}
}

// post sends one log: the answer's status (0 when there was none), its Retry-After, and the API's error message.
func (s *sender) post(body []byte) (status int, retryAfter, message string, err error) {
	req, err := http.NewRequest(http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return 0, "", "", err
	}
	req.Header = s.headers.Clone()
	res, err := s.http.Do(req)
	if err != nil {
		return 0, "", "", err
	}
	defer res.Body.Close()
	// Reading the body to its end lets net/http use the connection again
	answer, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	var parsed struct {
		Message string `json:"message"`
	}
	if res.StatusCode >= 300 && json.Unmarshal(answer, &parsed) == nil {
		message = parsed.Message
	}
	return res.StatusCode, res.Header.Get("Retry-After"), message, nil
}

// pause stops sending until the API's Retry-After (the start of the next month, for the monthly cap), dropping what's
// queued, with one warning for the whole pause.
func (s *sender) pause(retryAfter, message string) {
	wait := pauseFor
	if seconds, err := strconv.ParseFloat(retryAfter, 64); err == nil && seconds > 0 {
		wait = time.Duration(seconds * float64(time.Second))
	}
	until := time.Now().Add(wait)
	s.pausedUntil.Store(until.UnixNano())
	for drained := false; !drained; {
		select {
		case <-s.queue:
			s.pending.Add(-1)
		default:
			drained = true
		}
	}
	if message == "" {
		message = "Lumber is refusing logs for now (HTTP 429)."
	}
	s.warn(fmt.Sprintf("%s Dropping logs until %s.", message, until.UTC().Format(time.RFC3339)))
}
