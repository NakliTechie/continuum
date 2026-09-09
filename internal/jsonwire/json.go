// Package jsonwire encodes standalone JSON transport/storage documents.
// Its output must not be embedded directly into an HTML script element.
package jsonwire

import (
	"bytes"
	"encoding/json"
)

// Marshal preserves HTML-sensitive bytes in already-framed JSON payloads.
// Escaping those bytes again can multiply an accepted ACP frame's size by six.
func Marshal(value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
