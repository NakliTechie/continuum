package cli

import (
	"testing"

	"github.com/NakliTechie/continuum/internal/journal"
)

func TestParseRecording(t *testing.T) {
	for _, tc := range []struct {
		in    string
		mode  string
		lines int
	}{
		{"", journal.RecordingFull, 0},
		{"none", journal.RecordingNone, 0},
		{"visible", journal.RecordingVisible, 0},
		{"full", journal.RecordingFull, 0},
		{"lines:250", journal.RecordingLines, 250},
	} {
		mode, lines, err := parseRecording(tc.in)
		if err != nil || mode != tc.mode || lines != tc.lines {
			t.Fatalf("%q: %q %d %v", tc.in, mode, lines, err)
		}
	}
	for _, bad := range []string{"lines", "lines:nope", "lines:0", "lines:10001", "screen", "FULL"} {
		if _, _, err := parseRecording(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
