package platform

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

var ErrInvalidCursor = errors.New("invalid cursor")

func EncodeCursor(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func DecodeCursor(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", ErrInvalidCursor
	}
	return string(decoded), nil
}

// EncodeJSONCursor serializes a structured cursor into the same base64url
// envelope as EncodeCursor. Callers tag the payload so mixed cursor versions
// can be distinguished after decoding.
func EncodeJSONCursor(value any) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

// DecodeJSONCursor decodes a structured cursor produced by EncodeJSONCursor.
func DecodeJSONCursor(value string, target any) error {
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return ErrInvalidCursor
	}
	if err := json.Unmarshal(decoded, target); err != nil {
		return ErrInvalidCursor
	}
	return nil
}
