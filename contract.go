package lumber

import (
	"reflect"
	"strings"
	"time"
)

// Lumber-Kit's contract in Go: the constants, API key format, payload and delivery rules every Lumber plugin shares.
// testdata/contract holds the kit's schema and known answers, which everything here is tested against.

// DefaultEndpoint is the Lumber API logs go to unless New is given WithEndpoint.
const DefaultEndpoint = "https://api.lumber.infinitetoken.com"

const (
	ingestPath = "/logs"
	keyVersion = 1
	keyBytes   = 32
	nonceBytes = 12
	tagBytes   = 16

	maxAttempts = 5
	baseDelay   = time.Second
	maxDelay    = 30 * time.Second
	pauseFor    = time.Hour
)

// Lumber's log levels as the API numbers them.
const (
	wireDebug = 0
	wireInfo  = 1
	wireWarn  = 2
	wireError = 3
	wireFatal = 4
)

type outcome string

const (
	outcomeSent  outcome = "sent"
	outcomeRetry outcome = "retry"
	outcomeDrop  outcome = "drop"
	outcomePause outcome = "pause"
)

// splitAPIKey splits an API key, `<token>.<encryptionKey>`, into its halves. A value with no dot is all token.
func splitAPIKey(apiKey string) (token, encryptionKey string, hasKey bool) {
	return strings.Cut(strings.TrimSpace(apiKey), ".")
}

// buildPayload is the plaintext a log encrypts. requestId and traceId become the payload's own fields, the log's context
// goes over the automatic context, and every other key goes into metadata, where an explicit metadata key wins. A
// context or metadata that isn't a map is kept as an ordinary metadata key, and a nil one is left out.
func buildPayload(level int, message string, data, autoContext map[string]any) map[string]any {
	rest := make(map[string]any, len(data))
	for key, value := range data {
		rest[key] = value
	}
	requestID, traceID := take(rest, "requestId"), take(rest, "traceId")
	givenContext, givenMetadata := take(rest, "context"), take(rest, "metadata")

	context, isContext := asMap(givenContext)
	metadata, isMetadata := asMap(givenMetadata)
	if givenContext != nil && !isContext {
		rest["context"] = givenContext
	}
	if givenMetadata != nil && !isMetadata {
		rest["metadata"] = givenMetadata
	}

	payload := map[string]any{"level": level, "message": message}
	if requestID != nil {
		payload["requestId"] = requestID
	}
	if traceID != nil {
		payload["traceId"] = traceID
	}
	if len(autoContext) > 0 {
		payload["context"] = merge(autoContext, context)
	} else if isContext {
		payload["context"] = merge(context, nil)
	}
	if len(rest) > 0 {
		payload["metadata"] = merge(rest, metadata)
	} else if isMetadata {
		payload["metadata"] = merge(metadata, nil)
	}
	return payload
}

// deliveryOutcome is what to do with a log once POST /logs answers with status, or 0 for no answer (a network error).
func deliveryOutcome(status int) outcome {
	switch {
	case status == 0:
		return outcomeRetry
	case status >= 200 && status < 300:
		return outcomeSent
	case status == 429:
		return outcomePause
	case status >= 400 && status < 500:
		return outcomeDrop
	default:
		return outcomeRetry
	}
}

// retryDelay is how long to wait before trying a log again after its nth failure: 1s, 2s, 4s and so on, up to 30s.
func retryDelay(failedAttempts int) time.Duration {
	delay := baseDelay << max(0, failedAttempts-1)
	if delay <= 0 || delay > maxDelay {
		return maxDelay
	}
	return delay
}

func take(m map[string]any, key string) any {
	value := m[key]
	delete(m, key)
	return value
}

// asMap is a map with string keys of any value type as a map[string]any, or false for anything else (a nil map too).
func asMap(value any) (map[string]any, bool) {
	if m, ok := value.(map[string]any); ok {
		return m, m != nil
	}
	v := reflect.ValueOf(value)
	if v.Kind() != reflect.Map || v.Type().Key().Kind() != reflect.String || v.IsNil() {
		return nil, false
	}
	m := make(map[string]any, v.Len())
	for iter := v.MapRange(); iter.Next(); {
		m[iter.Key().String()] = iter.Value().Interface()
	}
	return m, true
}

func merge(base, over map[string]any) map[string]any {
	merged := make(map[string]any, len(base)+len(over))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range over {
		merged[key] = value
	}
	return merged
}
