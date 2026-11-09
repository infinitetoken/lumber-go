package lumber

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"testing"
	"time"
)

func TestContractConstants(t *testing.T) {
	c := vectors.Constants
	if c.DefaultEndpoint != DefaultEndpoint || c.IngestPath != ingestPath || c.KeyVersion != keyVersion {
		t.Errorf("endpoint, path or key version differ from the kit's: %+v", c)
	}
	if c.KeyBytes != keyBytes || c.NonceBytes != nonceBytes || c.TagBytes != tagBytes {
		t.Errorf("key, nonce or tag sizes differ from the kit's: %+v", c)
	}
	levels := map[string]int{"debug": wireDebug, "info": wireInfo, "warn": wireWarn, "error": wireError, "fatal": wireFatal}
	equalJSON(t, levels, c.Levels)
	d := c.Delivery
	if d.MaxAttempts != maxAttempts || time.Duration(d.BaseDelayMs)*time.Millisecond != baseDelay ||
		time.Duration(d.MaxDelayMs)*time.Millisecond != maxDelay || time.Duration(d.PauseMs)*time.Millisecond != pauseFor {
		t.Errorf("delivery differs from the kit's: %+v", d)
	}
}

func TestContractAPIKeys(t *testing.T) {
	for _, v := range vectors.APIKeys {
		t.Run(v.Name, func(t *testing.T) {
			token, key, hasKey := splitAPIKey(v.APIKey)
			if token != v.Token {
				t.Errorf("token %q, want %q", token, v.Token)
			}
			if v.EncryptionKey == nil && hasKey || v.EncryptionKey != nil && (!hasKey || key != *v.EncryptionKey) {
				t.Errorf("encryption key %q (%v), want %v", key, hasKey, v.EncryptionKey)
			}
		})
	}
}

func TestContractEncryption(t *testing.T) {
	for _, v := range vectors.Encryption {
		t.Run(v.Name, func(t *testing.T) {
			key, _ := base64.StdEncoding.DecodeString(v.Key)
			nonce, _ := base64.StdEncoding.DecodeString(v.Nonce)
			block, _ := aes.NewCipher(key)
			aead, _ := cipher.NewGCM(block)
			if got := base64.StdEncoding.EncodeToString(seal(aead, []byte(v.Plaintext), nonce)); got != v.Payload {
				t.Errorf("payload %s, want %s", got, v.Payload)
			}
		})
	}
}

func TestContractPayloads(t *testing.T) {
	for _, v := range vectors.Payloads {
		t.Run(v.Name, func(t *testing.T) {
			payload := buildPayload(v.Level, v.Message, v.Data, v.AutoContext)
			equalJSON(t, payload, v.Payload)
			conforms(t, "LogPayload", payload)
		})
	}
}

// Through the whole client: what it sends is a LogIngestBody whose plaintext is the vector's payload. The data goes in
// with the API's names, which the client takes as they are.
func TestContractClientSendsPayloads(t *testing.T) {
	for _, v := range vectors.Payloads {
		if v.AutoContext != nil {
			continue
		}
		t.Run(v.Name, func(t *testing.T) {
			s := newServer(t)
			client, _ := newTestClient(t, s)
			data := map[string]any{}
			for key, value := range v.Data {
				data[key] = value
			}
			client.send(v.Level, v.Message, data, nil)
			flush(t, client)

			body := s.body(0)
			conforms(t, "LogIngestBody", body)
			encrypted := body["encrypted"].(map[string]any)
			if body["level"] != float64(v.Level) || encrypted["keyVersion"] != float64(vectors.Constants.KeyVersion) {
				t.Errorf("level or key version: %v", body)
			}
			if nonce, _ := base64.StdEncoding.DecodeString(encrypted["nonce"].(string)); len(nonce) != vectors.Constants.NonceBytes {
				t.Errorf("nonce is %d bytes", len(nonce))
			}
			equalJSON(t, decrypt(t, body), v.Payload)
		})
	}
}

func TestContractDeliveryOutcomes(t *testing.T) {
	for _, v := range vectors.DeliveryOutcomes {
		status := 0
		if v.Status != nil {
			status = *v.Status
		}
		if got := deliveryOutcome(status); string(got) != v.Outcome {
			t.Errorf("status %d: %s, want %s", status, got, v.Outcome)
		}
	}
}

func TestContractRetryDelays(t *testing.T) {
	for _, v := range vectors.RetryDelaysMs {
		if got := retryDelay(v.FailedAttempts); got != time.Duration(v.DelayMs)*time.Millisecond {
			t.Errorf("after %d failures: %v, want %dms", v.FailedAttempts, got, v.DelayMs)
		}
	}
	if retryDelay(200) != maxDelay {
		t.Error("a delay past the cap should be the cap")
	}
}
