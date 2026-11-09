package lumber

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"
)

// Lumber-Kit's contract (testdata/contract, synced by scripts/sync-contract.sh): the same known answers Lumber-Node and
// Lumber-Python are tested against.
var vectors struct {
	Constants struct {
		DefaultEndpoint string         `json:"defaultEndpoint"`
		IngestPath      string         `json:"ingestPath"`
		KeyVersion      int            `json:"keyVersion"`
		KeyBytes        int            `json:"keyBytes"`
		NonceBytes      int            `json:"nonceBytes"`
		TagBytes        int            `json:"tagBytes"`
		Levels          map[string]int `json:"levels"`
		Delivery        struct {
			MaxAttempts int `json:"maxAttempts"`
			BaseDelayMs int `json:"baseDelayMs"`
			MaxDelayMs  int `json:"maxDelayMs"`
			PauseMs     int `json:"pauseMs"`
		} `json:"delivery"`
	} `json:"constants"`
	APIKeys []struct {
		Name, APIKey, Token string
		EncryptionKey       *string
	} `json:"apiKeys"`
	Encryption []struct {
		Name, Key, Nonce, Plaintext, Payload string
	} `json:"encryption"`
	Payloads []struct {
		Name        string
		Level       int
		Message     string
		Data        map[string]any
		AutoContext map[string]any
		Payload     map[string]any
	} `json:"payloads"`
	DeliveryOutcomes []struct {
		Status  *int
		Outcome string
	} `json:"deliveryOutcomes"`
	RetryDelaysMs []struct {
		FailedAttempts int
		DelayMs        int
	} `json:"retryDelaysMs"`
}

var schema struct {
	Definitions map[string]map[string]any `json:"definitions"`
}

var testKey string

func TestMain(m *testing.M) {
	for name, target := range map[string]any{"vectors.json": &vectors, "schema.json": &schema} {
		raw, err := os.ReadFile("testdata/contract/" + name)
		if err == nil {
			err = json.Unmarshal(raw, target)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "reading the contract:", err)
			os.Exit(1)
		}
	}
	testKey = vectors.Encryption[0].Key
	os.Exit(m.Run())
}

const testToken = "3f9c0e7a1b2d4c6e8f0a1b2c3d4e5f60"

func testAPIKey() string { return testToken + "." + testKey }

// server stands in for the Lumber API: it records each request and answers from a script (201 once it runs out).
type server struct {
	*httptest.Server
	mu      sync.Mutex
	bodies  []map[string]any
	headers []http.Header
	answers []func(http.ResponseWriter)
	conns   map[string]bool
	release chan struct{}
	arrived chan struct{} // one per request as it comes in, before hold holds it
}

func newServer(t *testing.T) *server {
	t.Helper()
	s := &server{conns: map[string]bool{}, arrived: make(chan struct{}, 1000)}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/logs" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		s.arrived <- struct{}{}
		s.mu.Lock()
		release := s.release
		s.mu.Unlock()
		if release != nil {
			<-release
		}
		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		s.headers = append(s.headers, r.Header.Clone())
		s.conns[r.RemoteAddr] = true
		var answer func(http.ResponseWriter)
		if len(s.answers) > 0 {
			answer, s.answers = s.answers[0], s.answers[1:]
		}
		s.mu.Unlock()
		if answer != nil {
			answer(w)
		} else {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"data":{}}`))
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) answer(status int, retryAfter, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers = append(s.answers, func(w http.ResponseWriter) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
		if message != "" {
			_, _ = fmt.Fprintf(w, `{"message":%q}`, message)
		}
	})
}

func (s *server) hold() {
	s.mu.Lock()
	s.release = make(chan struct{})
	s.mu.Unlock()
}

func (s *server) let() {
	s.mu.Lock()
	release := s.release
	s.release = nil
	s.mu.Unlock()
	close(release)
}

// waitArrived waits for the next request to reach the server, held or not.
func (s *server) waitArrived(t *testing.T) {
	t.Helper()
	select {
	case <-s.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("no request reached the server")
	}
}

func (s *server) body(i int) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies[i]
}

func (s *server) header(i int) http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.headers[i]
}

func (s *server) connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// logs are the decrypted payloads the server received.
func (s *server) logs(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	logs := make([]map[string]any, len(s.bodies))
	for i, body := range s.bodies {
		logs[i] = decrypt(t, body)
	}
	return logs
}

func (s *server) messages(t *testing.T) []string {
	t.Helper()
	var messages []string
	for _, log := range s.logs(t) {
		messages = append(messages, log["message"].(string))
	}
	return messages
}

type warnings struct {
	mu   sync.Mutex
	seen []string
}

func (w *warnings) add(message string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seen = append(w.seen, message)
}

func (w *warnings) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.seen...)
}

// newTestClient sends to s without automatic context, retrying at once rather than after 1s, 2s, 4s.
func newTestClient(t *testing.T, s *server, opts ...Option) (*Client, *warnings) {
	t.Helper()
	w := &warnings{}
	client, err := New(testAPIKey(), append([]Option{WithEndpoint(s.URL), WithoutAutoContext(), WithWarnings(w.add)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	client.sender.retryWait = func(int) time.Duration { return 0 }
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	return client, w
}

func flush(t *testing.T, client *Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}

func decrypt(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	encrypted := body["encrypted"].(map[string]any)
	nonce, _ := base64.StdEncoding.DecodeString(encrypted["nonce"].(string))
	sealed, _ := base64.StdEncoding.DecodeString(encrypted["payload"].(string))
	key, _ := base64.StdEncoding.DecodeString(testKey)
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	plaintext, err := aead.Open(nil, nonce, append(append([]byte{}, sealed[tagBytes:]...), sealed[:tagBytes]...), nil)
	if err != nil {
		t.Fatalf("decrypting: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		t.Fatalf("decrypted JSON: %v", err)
	}
	return payload
}

// normalize round-trips a value through JSON, so values compare as they'd arrive (every number a float64).
func normalize(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}

func equalJSON(t *testing.T, got, want any) {
	t.Helper()
	if g, w := normalize(t, got), normalize(t, want); !reflect.DeepEqual(g, w) {
		t.Errorf("got  %v\nwant %v", g, w)
	}
}

// conforms checks a value against one of the kit's schema definitions: the subset of JSON Schema the generated schema
// uses ($ref, type, enum, properties, required, additionalProperties), so the module needs no dependency for it.
func conforms(t *testing.T, definition string, value any) {
	t.Helper()
	if problem := check(schema.Definitions[definition], normalize(t, value), definition); problem != "" {
		t.Errorf("not a %s: %s", definition, problem)
	}
}

func check(rule map[string]any, value any, path string) string {
	if ref, ok := rule["$ref"].(string); ok {
		name := ref[len("#/definitions/"):]
		return check(schema.Definitions[name], value, path)
	}
	if enum, ok := rule["enum"].([]any); ok {
		found := false
		for _, allowed := range enum {
			found = found || reflect.DeepEqual(allowed, value)
		}
		if !found {
			return fmt.Sprintf("%s is %v, not one of %v", path, value, enum)
		}
	}
	switch rule["type"] {
	case "string":
		if _, ok := value.(string); !ok {
			return path + " isn't a string"
		}
	case "number":
		if _, ok := value.(float64); !ok {
			return path + " isn't a number"
		}
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return path + " isn't an object"
		}
		properties, _ := rule["properties"].(map[string]any)
		required, _ := rule["required"].([]any)
		for _, key := range required {
			if _, ok := object[key.(string)]; !ok {
				return fmt.Sprintf("%s has no %s", path, key)
			}
		}
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if property, ok := properties[key].(map[string]any); ok {
				if problem := check(property, object[key], path+"."+key); problem != "" {
					return problem
				}
			} else if rule["additionalProperties"] == false {
				return fmt.Sprintf("%s has %s, which it doesn't allow", path, key)
			}
		}
	}
	return ""
}
