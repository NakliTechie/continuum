package legacy

import (
	"strings"
	"testing"
)

// A binary path inside a double-quoted systemd argument must not change the
// command: quotes and backslashes are escaped, % and $ would otherwise be
// expanded as specifiers and variables.
func TestSystemdQuotedEscapesSpecialCharacters(t *testing.T) {
	got := systemdQuoted(`/opt/build%20cache/it's "here"/$HOME\bin/continuum`)
	want := `/opt/build%%20cache/it's \"here\"/$$HOME\\bin/continuum`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// Every % and $ in the output is doubled: nothing single is left for systemd to expand.
	if strings.Count(got, "%")%2 != 0 || strings.Count(got, "$")%2 != 0 {
		t.Fatal("specifier or variable left unescaped")
	}
}
