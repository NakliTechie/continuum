// Package asciicast writes an asciicast v3 stream from Continuum's journaled
// PTY output. v3 is line-oriented: a header object, then one array per event
// `[interval, code, data]` where interval is seconds since the previous event.
// A recording gap — retired history, or an unclean epoch — is emitted as an
// `m` (marker) event rather than silently closing the time skip, so a player
// shows the discontinuity instead of pretending the bytes were contiguous.
package asciicast

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Header is the first line of a v3 stream.
type Header struct {
	Version   int            `json:"version"`
	Term      Term           `json:"term"`
	Timestamp int64          `json:"timestamp,omitempty"`
	Title     string         `json:"title,omitempty"`
	Env       map[string]any `json:"env,omitempty"`
}

// Term carries the initial terminal geometry (required in v3).
type Term struct {
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
	Type string `json:"type,omitempty"`
}

// Writer emits a v3 stream to w. Intervals are computed from the wall-clock
// timestamps handed to Output/Marker, clamped non-negative.
type Writer struct {
	w        io.Writer
	enc      *json.Encoder
	last     time.Time
	hasFirst bool
}

// NewWriter writes the header and returns a Writer for the event lines.
func NewWriter(w io.Writer, h Header) (*Writer, error) {
	h.Version = 3
	if h.Term.Cols <= 0 {
		h.Term.Cols = 80
	}
	if h.Term.Rows <= 0 {
		h.Term.Rows = 24
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(h); err != nil {
		return nil, err
	}
	return &Writer{w: w, enc: enc}, nil
}

func (aw *Writer) interval(at time.Time) float64 {
	if !aw.hasFirst {
		aw.hasFirst = true
		aw.last = at
		return 0
	}
	d := at.Sub(aw.last).Seconds()
	if d < 0 {
		d = 0
	}
	aw.last = at
	return d
}

// Output writes a printed-output ("o") event at time at.
func (aw *Writer) Output(at time.Time, data []byte) error {
	return aw.event(aw.interval(at), "o", string(data))
}

// Marker writes a marker ("m") event — a labelled discontinuity. Its interval
// is zero so it does not advance playback time across the gap it names.
func (aw *Writer) Marker(label string) error {
	return aw.event(0, "m", label)
}

func (aw *Writer) event(interval float64, code, data string) error {
	// asciicast events are 3-element arrays: a fixed-precision interval and two
	// JSON strings. Build it by hand so the interval keeps its own formatting.
	cb, err := json.Marshal(code)
	if err != nil {
		return err
	}
	db, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(aw.w, "[%s, %s, %s]\n", trimFloat(interval), cb, db)
	return err
}

// trimFloat renders seconds with microsecond resolution and no trailing zeros.
func trimFloat(f float64) string {
	s := fmt.Sprintf("%.6f", f)
	i := len(s)
	for i > 0 && s[i-1] == '0' {
		i--
	}
	if i > 0 && s[i-1] == '.' {
		i--
	}
	if i == 0 {
		return "0"
	}
	return s[:i]
}
