package lumber

import (
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestHandlerLevels(t *testing.T) {
	cases := map[slog.Level]int{
		slog.LevelDebug - 4: wireDebug, slog.LevelDebug: wireDebug, slog.LevelInfo: wireInfo, slog.LevelInfo + 2: wireInfo,
		slog.LevelWarn: wireWarn, slog.LevelError: wireError, slog.LevelError + 2: wireError, LevelFatal: wireFatal, LevelFatal + 4: wireFatal,
	}
	for level, want := range cases {
		if got := wireLevel(level); got != want {
			t.Errorf("%v: %d, want %d", level, got, want)
		}
	}
}

func TestHandlerRecord(t *testing.T) {
	s := newServer(t)
	client, _ := newTestClient(t, s)
	logger := slog.New(client.Handler(&HandlerOptions{AddSource: true}))
	logger.Warn("Rate limit", "request_id", "req-1", "user_id", 7, slog.Group("context", "region", "us"))
	flush(t, client)
	log := s.logs(t)[0]
	context := log["context"].(map[string]any)
	if !strings.HasSuffix(context["code"].(string), "handler_test.go") || context["method"] != "github.com/infinitetoken/lumber-go.TestHandlerRecord" {
		t.Errorf("context %v", context)
	}
	delete(log, "context")
	equalJSON(t, log, map[string]any{"level": 2, "message": "Rate limit", "requestId": "req-1", "metadata": map[string]any{"user_id": 7}})
	if context["region"] != "us" {
		t.Errorf("region %v", context["region"])
	}
}

func TestHandlerGroupsAndAttrs(t *testing.T) {
	s := newServer(t)
	client, _ := newTestClient(t, s)
	logger := slog.New(client.Handler(nil)).With("service", "billing").WithGroup("job").With("id", 42)
	logger.Info("Done", "rows", 1500, slog.Group("timing", "ms", 12), slog.Group("empty"), slog.Attr{})
	logger.Info("Nothing")
	slog.New(client.Handler(nil)).WithGroup("unused").Info("Bare")
	flush(t, client)
	logs := s.logs(t)
	equalJSON(t, logs[0]["metadata"], map[string]any{"service": "billing", "job": map[string]any{"id": 42, "rows": 1500, "timing": map[string]any{"ms": 12}}})
	equalJSON(t, logs[1]["metadata"], map[string]any{"service": "billing", "job": map[string]any{"id": 42}})
	equalJSON(t, logs[2], map[string]any{"level": 1, "message": "Bare"})
}

func TestHandlerLevel(t *testing.T) {
	s := newServer(t)
	client, _ := newTestClient(t, s)
	logger := slog.New(client.Handler(nil))
	logger.Debug("skipped")
	logger.Log(context.Background(), LevelFatal, "sent")
	debug := slog.New(client.Handler(&HandlerOptions{Level: slog.LevelDebug}))
	debug.Debug("also sent")
	flush(t, client)
	equalJSON(t, s.messages(t), []string{"sent", "also sent"})
	if s.logs(t)[0]["level"] != float64(wireFatal) {
		t.Error("LevelFatal wasn't FATAL")
	}
}
