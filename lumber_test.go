package lumber

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNewRefusesABadAPIKey(t *testing.T) {
	short := make([]byte, 16)
	_, _ = rand.Read(short)
	cases := map[string]string{
		"":              "missing API key",
		"   ":           "missing API key",
		testToken:       "no encryption key",
		testToken + ".": "no encryption key",
		testToken + "." + base64.StdEncoding.EncodeToString(short): "32 bytes",
		testToken + ".not base64!":                                 "32 bytes",
	}
	for apiKey, want := range cases {
		if _, err := New(apiKey); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("New(%q): %v, want %q", apiKey, err, want)
		}
	}
	if _, err := New(testAPIKey(), WithEndpoint("api.lumber.example")); err == nil || !strings.Contains(err.Error(), "endpoint") {
		t.Errorf("a bad endpoint: %v", err)
	}
}

func TestSendsOnlyTheToken(t *testing.T) {
	s := newServer(t)
	w := &warnings{}
	client, err := New("  "+testAPIKey()+"\n", WithEndpoint(s.URL+"/"), WithWarnings(w.add))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(context.Background())
	client.Info("hello")
	flush(t, client)
	header := s.header(0)
	if got := header.Get("Authorization"); got != "Bearer "+testToken {
		t.Errorf("Authorization %q", got)
	}
	if got := header.Get("User-Agent"); !strings.HasPrefix(got, "lumber-go/"+Version+" go") {
		t.Errorf("User-Agent %q", got)
	}
	if payload := s.body(0)["encrypted"].(map[string]any)["payload"].(string); strings.Contains(payload, "hello") || strings.Contains(payload, testKey) {
		t.Error("the message or key went out unencrypted")
	}
}

func TestLevels(t *testing.T) {
	s := newServer(t)
	client, _ := newTestClient(t, s)
	client.Debug("d")
	client.Info("i")
	client.Warn("w")
	client.Error("e")
	client.Fatal("f")
	flush(t, client)
	var levels []float64
	for _, log := range s.logs(t) {
		levels = append(levels, log["level"].(float64))
	}
	equalJSON(t, levels, []int{0, 1, 2, 3, 4})
}

func TestArguments(t *testing.T) {
	s := newServer(t)
	client, _ := newTestClient(t, s)
	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	client.Error("Payment failed",
		"request_id", "req-1",
		"trace_id", "trace-1",
		"order_id", 789,
		"err", errors.New("card declined"),
		"at", at,
		"took", 1500*time.Millisecond,
		"tags", []string{"a", "b"},
		"ratio", math.NaN(),
		"context", map[string]string{"region": "us"},
	)
	client.Info("none", "request_id", nil)
	flush(t, client)
	logs := s.logs(t)
	equalJSON(t, logs[0], map[string]any{
		"level": 3, "message": "Payment failed", "requestId": "req-1", "traceId": "trace-1",
		"context": map[string]any{"region": "us"},
		"metadata": map[string]any{
			"order_id": 789, "err": "card declined", "at": "2026-11-02T09:30:00Z", "took": "1.5s", "tags": []string{"a", "b"}, "ratio": nil,
		},
	})
	equalJSON(t, logs[1], map[string]any{"level": 1, "message": "none"})
}

func TestAutomaticContext(t *testing.T) {
	s := newServer(t)
	client, err := New(testAPIKey(), WithEndpoint(s.URL), WithEnvironment("production"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(context.Background())
	client.Info("hello", "context", map[string]any{"region": "us"})
	flush(t, client)
	hostname, _ := os.Hostname()
	equalJSON(t, s.logs(t)[0]["context"], map[string]any{"pid": os.Getpid(), "hostname": hostname, "env": "production", "region": "us"})
}

func TestCaller(t *testing.T) {
	s := newServer(t)
	client, _ := newTestClient(t, s, WithCaller(0))
	wrapped, _ := newTestClient(t, s, WithCaller(1))
	wrapper := func(msg string) { wrapped.Info(msg) }

	client.Info("direct")
	wrapper("wrapped")
	flush(t, client)
	flush(t, wrapped)
	for _, log := range s.logs(t) {
		context := log["context"].(map[string]any)
		if !strings.HasSuffix(context["code"].(string), "lumber_test.go") || context["method"] != "github.com/infinitetoken/lumber-go.TestCaller" {
			t.Errorf("%s: context %v", log["message"], context)
		}
	}
}

func TestCloseDropsLaterLogs(t *testing.T) {
	s := newServer(t)
	client, _ := newTestClient(t, s)
	client.Info("before")
	if err := client.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.Info("after")
	equalJSON(t, s.messages(t), []string{"before"})
}
