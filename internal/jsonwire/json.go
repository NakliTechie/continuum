// Package jsonwire encodes standalone JSON transport/storage documents.
// Its output must not be embedded directly into an HTML script element.
package jsonwire

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
)

// Marshal preserves HTML-sensitive bytes in already-framed JSON payloads.
// Escaping those bytes again can multiply an accepted ACP frame's size by six.
//
// Embedded json.RawMessage values keep their bytes (compacted, as encoding/json
// always did). Go 1.27's encoding/json escapes U+2028 and U+2029 inside a
// RawMessage even with HTML escaping off, which rewrites an agent's payload;
// when that happens, Marshal splices the compacted raw bytes back in place.
func Marshal(value any) ([]byte, error) {
	out, err := encode(value)
	if err != nil {
		return nil, err
	}
	if !bytes.Contains(out, []byte("\\u2028")) && !bytes.Contains(out, []byte("\\u2029")) {
		return out, nil
	}
	return marshalSplicing(value, out)
}

func encode(value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

var (
	rawType = reflect.TypeFor[json.RawMessage]()
	lsep    = []byte("\u2028")
	psep    = []byte("\u2029")
)

const maxDepth = 64

type splicer struct {
	nonce string
	raws  [][]byte // compacted raw values, indexed by placeholder number
	deep  bool     // gave up: structure deeper than maxDepth
}

// marshalSplicing encodes a copy of value in which every RawMessage that holds
// U+2028/U+2029 is a unique placeholder string, then replaces each placeholder
// with the compacted raw bytes. The caller's value is never modified.
func marshalSplicing(value any, fallback []byte) ([]byte, error) {
	var n [16]byte
	if _, err := rand.Read(n[:]); err != nil {
		return fallback, nil
	}
	s := &splicer{nonce: hex.EncodeToString(n[:])}
	v := reflect.ValueOf(value)
	if !v.IsValid() {
		return fallback, nil
	}
	cp := s.copy(v, 0)
	if s.deep || len(s.raws) == 0 {
		return fallback, nil
	}
	out, err := encode(cp.Interface())
	if err != nil {
		return nil, err
	}
	for i, raw := range s.raws {
		ph := []byte(strconv.Quote(s.placeholder(i)))
		if bytes.Count(out, ph) != 1 {
			return fallback, nil
		}
		out = bytes.Replace(out, ph, raw, 1)
	}
	if !json.Valid(out) {
		return fallback, nil
	}
	return out, nil
}

func (s *splicer) placeholder(i int) string {
	return fmt.Sprintf("jsonwire-splice-%s-%d", s.nonce, i)
}

// copy returns a copy of v with qualifying RawMessage values replaced. Only
// what encoding/json would visit is rewritten: exported struct fields, map
// values, slice and array elements, pointers and interfaces.
func (s *splicer) copy(v reflect.Value, depth int) reflect.Value {
	if depth > maxDepth {
		s.deep = true
		return v
	}
	if v.Type() == rawType {
		raw := v.Bytes()
		if !bytes.Contains(raw, lsep) && !bytes.Contains(raw, psep) {
			return v
		}
		var c bytes.Buffer
		if err := json.Compact(&c, raw); err != nil {
			return v // invalid raw: let encoding/json report it
		}
		s.raws = append(s.raws, c.Bytes())
		ph, _ := json.Marshal(s.placeholder(len(s.raws) - 1))
		out := reflect.New(rawType).Elem()
		out.SetBytes(ph)
		return out
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(s.copy(v.Elem(), depth+1))
		return out
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		inner := s.copy(v.Elem(), depth+1)
		out := reflect.New(v.Type()).Elem()
		out.Set(inner)
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for i := 0; i < v.NumField(); i++ {
			if !v.Type().Field(i).IsExported() {
				continue
			}
			out.Field(i).Set(s.copy(v.Field(i), depth+1))
		}
		return out
	case reflect.Slice:
		if v.IsNil() || v.Type().Elem().Kind() == reflect.Uint8 {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(s.copy(v.Index(i), depth+1))
		}
		return out
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(s.copy(v.Index(i), depth+1))
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		it := v.MapRange()
		for it.Next() {
			out.SetMapIndex(it.Key(), s.copy(it.Value(), depth+1))
		}
		return out
	}
	return v
}
