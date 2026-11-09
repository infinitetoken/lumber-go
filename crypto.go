package lumber

import (
	"bytes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
)

type encryptedLog struct {
	Nonce      string `json:"nonce"`
	Payload    string `json:"payload"`
	KeyVersion int    `json:"keyVersion"`
}

// ingestBody is POST /logs' body: the level stays plaintext for filtering and pushes, everything else is encrypted.
type ingestBody struct {
	Level     int          `json:"level"`
	Encrypted encryptedLog `json:"encrypted"`
}

// seal is AES-256-GCM in Lumber's layout: the 16-byte auth tag, then the ciphertext. Go puts the tag last.
func seal(aead cipher.AEAD, plaintext, nonce []byte) []byte {
	sealed := aead.Seal(nil, nonce, plaintext, nil)
	split := len(sealed) - tagBytes
	return append(append(make([]byte, 0, len(sealed)), sealed[split:]...), sealed[:split]...)
}

// encrypt seals a payload under a fresh random nonce. Never reuse a nonce with the same key.
func encrypt(aead cipher.AEAD, payload map[string]any) (encryptedLog, error) {
	plaintext, err := toJSON(payload)
	if err != nil {
		return encryptedLog{}, err
	}
	nonce := make([]byte, nonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return encryptedLog{}, err
	}
	return encryptedLog{
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Payload:    base64.StdEncoding.EncodeToString(seal(aead, plaintext, nonce)),
		KeyVersion: keyVersion,
	}, nil
}

// toJSON is compact JSON the Lumber apps can always parse. When a value can't be encoded (a NaN, which isn't JSON, a
// channel, a map that contains itself) it's cleaned first, rather than a log being lost.
func toJSON(value any) ([]byte, error) {
	encoded, err := marshal(value)
	if err != nil {
		encoded, err = marshal(clean(value, map[uintptr]bool{}, 0))
	}
	return encoded, err
}

func marshal(value any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// jsonValue is a value as it should be sent: an error as its message, since encoding/json would send most errors as {}.
func jsonValue(value any) any {
	switch v := value.(type) {
	case error:
		return v.Error()
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = jsonValue(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = jsonValue(item)
		}
		return out
	default:
		return value
	}
}

func clean(value any, seen map[uintptr]bool, depth int) any {
	if depth > 64 {
		return "[Too deep]"
	}
	switch v := value.(type) {
	case nil, string, bool, json.Number:
		return v
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil
		}
		return v
	case float32:
		return clean(float64(v), seen, depth)
	case error:
		return v.Error()
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Map, reflect.Slice:
		if rv.IsNil() {
			return nil
		}
		if rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8 {
			return value
		}
		if seen[rv.Pointer()] {
			return "[Circular]"
		}
		seen[rv.Pointer()] = true
		defer delete(seen, rv.Pointer())
		if rv.Kind() == reflect.Map {
			out := make(map[string]any, rv.Len())
			for iter := rv.MapRange(); iter.Next(); {
				out[fmt.Sprint(iter.Key().Interface())] = clean(iter.Value().Interface(), seen, depth+1)
			}
			return out
		}
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = clean(rv.Index(i).Interface(), seen, depth+1)
		}
		return out
	case reflect.Array:
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = clean(rv.Index(i).Interface(), seen, depth+1)
		}
		return out
	}
	if _, err := marshal(value); err == nil {
		return value
	}
	return fmt.Sprintf("%+v", value)
}
