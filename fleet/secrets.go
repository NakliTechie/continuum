package fleet

import (
	"bytes"
	"fmt"
	"regexp"
)

// Secrets are referenced, never stored (D4): the spec is committed to the repo,
// so a value that looks like a credential is a validation failure, not a
// warning. Values resolve at materialise time from a source the user controls —
// an environment variable on the relay's own box.
//
// Each pattern is deliberately shaped like the credential it catches rather than
// like "anything long and random", so a legitimate opaque string (a cache key, a
// hash) does not trip it.
var secretPatterns = []struct {
	code string
	what string
	re   *regexp.Regexp
}{
	{"aws_access_key", "an AWS access key id", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"openai_key", "an OpenAI-style API key", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}\b`)},
	{"github_token", "a GitHub token", regexp.MustCompile(`\bghp_[A-Za-z0-9]{20,}\b`)},
	{"inline_db_credentials", "inline database credentials", regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@"]+:[^/\s:@"]+@`)},
}

// LintSecrets reports credential-shaped strings anywhere in the raw document.
// It runs on bytes rather than on the parsed Spec so that a credential in a
// field this version does not model is still caught.
func LintSecrets(b []byte) []Issue {
	var out []Issue
	// bytes.Split, not bufio.Scanner: the scanner has a token cap, and a line over
	// it ended the loop as though the document were fully read — so a credential
	// on or after an over-long line was silently missed and the spec passed. A
	// lint that cannot finish must never look like a lint that passed.
	for i, lineBytes := range bytes.Split(b, []byte("\n")) {
		line := i + 1
		text := string(lineBytes)
		for _, p := range secretPatterns {
			if p.re.MatchString(text) {
				out = append(out, Issue{
					Path: fmt.Sprintf("/#line=%d", line),
					Code: "secret_in_spec",
					Message: "line " + fmt.Sprint(line) + " looks like " + p.what +
						"; the spec is committed, so secrets are referenced, never stored (D4) — " +
						"resolve it at materialise time from an environment variable on the relay's box",
				})
			}
		}
	}
	return out
}
