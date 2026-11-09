package lumber

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestARefusedLogIsDroppedAtOnce(t *testing.T) {
	s := newServer(t)
	client, w := newTestClient(t, s)
	s.answer(401, "", "Invalid API key")
	client.Info("refused")
	client.Info("next")
	flush(t, client)
	equalJSON(t, s.messages(t), []string{"refused", "next"})
	equalJSON(t, w.all(), []string{"log refused: HTTP 401 (Invalid API key)"})
}

func TestAFailureIsTriedAgain(t *testing.T) {
	s := newServer(t)
	client, w := newTestClient(t, s)
	s.answer(503, "", "")
	s.answer(502, "", "")
	client.Info("flaky")
	client.Info("next")
	flush(t, client)
	equalJSON(t, s.messages(t), []string{"flaky", "flaky", "flaky", "next"})
	equalJSON(t, w.all(), []string{"send failed: HTTP 503", "send failed: HTTP 502"})
}

func TestALogIsDroppedAfterFiveFailures(t *testing.T) {
	s := newServer(t)
	client, w := newTestClient(t, s)
	for range 5 {
		s.answer(500, "", "")
	}
	client.Info("doomed")
	client.Info("next")
	flush(t, client)
	equalJSON(t, s.messages(t), []string{"doomed", "doomed", "doomed", "doomed", "doomed", "next"})
	if all := w.all(); all[len(all)-1] != "dropping log after 5 failed attempts" {
		t.Errorf("warnings %v", all)
	}
}

func TestNoServer(t *testing.T) {
	w := &warnings{}
	client, _ := New(testAPIKey(), WithEndpoint("http://127.0.0.1:9"), WithWarnings(w.add))
	client.sender.retryWait = func(int) time.Duration { return 0 }
	client.Info("nowhere")
	flush(t, client)
	all := w.all()
	if len(all) != 6 || !strings.HasPrefix(all[0], "send error: ") || all[5] != "dropping log after 5 failed attempts" {
		t.Errorf("warnings %v", all)
	}
}

func TestTheMonthlyCapPausesSending(t *testing.T) {
	s := newServer(t)
	client, w := newTestClient(t, s)
	s.hold()
	s.answer(429, "86400", "This team has sent its 10,000 logs for the month.")
	client.Info("capped")
	<-time.After(20 * time.Millisecond)
	client.Info("queued behind it")
	s.let()
	flush(t, client)
	client.Info("during the pause")
	flush(t, client)
	equalJSON(t, s.messages(t), []string{"capped"})
	all := w.all()
	if len(all) != 1 || !strings.HasPrefix(all[0], "This team has sent its 10,000 logs for the month. Dropping logs until ") {
		t.Errorf("warnings %v", all)
	}
	if wait := time.Until(time.Unix(0, client.sender.pausedUntil.Load())); wait < 86390*time.Second || wait > 86400*time.Second {
		t.Errorf("paused for %v", wait)
	}

	client.sender.pausedUntil.Store(0)
	client.Info("next month")
	flush(t, client)
	equalJSON(t, s.messages(t), []string{"capped", "next month"})
}

func TestAPauseWithoutRetryAfterLastsAnHour(t *testing.T) {
	s := newServer(t)
	client, w := newTestClient(t, s)
	s.answer(429, "", "")
	client.Info("capped")
	flush(t, client)
	if wait := time.Until(time.Unix(0, client.sender.pausedUntil.Load())); wait < 59*time.Minute || wait > time.Hour {
		t.Errorf("paused for %v", wait)
	}
	if all := w.all(); !strings.HasPrefix(all[0], "Lumber is refusing logs for now (HTTP 429). Dropping logs until ") {
		t.Errorf("warnings %v", all)
	}
}

func TestFlushGivesUpWithItsContext(t *testing.T) {
	s := newServer(t)
	client, _ := newTestClient(t, s)
	s.hold()
	client.Info("stuck")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := client.Flush(ctx); err != context.DeadlineExceeded {
		t.Errorf("Flush: %v", err)
	}
	s.let()
	flush(t, client)
}

func TestAFullQueueDropsNewLogs(t *testing.T) {
	s := newServer(t)
	client, w := newTestClient(t, s, WithMaxQueue(1))
	s.hold()
	// The first log is held at the server, so the queue's one place is free for the second and every one after it
	// is dropped. Logging them all at once would let the queue fill and empty again, warning twice.
	client.Info("log", "i", 0)
	s.waitArrived(t)
	for i := 1; i < 10; i++ {
		client.Info("log", "i", i)
	}
	s.let()
	flush(t, client)
	if got := len(s.messages(t)); got != 2 {
		t.Errorf("%d logs sent, want 2", got)
	}
	equalJSON(t, w.all(), []string{"dropping logs: 1 are already waiting to send"})
}

func TestTheConnectionIsKeptOpen(t *testing.T) {
	s := newServer(t)
	client, _ := newTestClient(t, s)
	for i := range 5 {
		client.Info("log", "i", i)
	}
	flush(t, client)
	if got := s.connections(); got != 1 {
		t.Errorf("%d connections for 5 logs", got)
	}
}
