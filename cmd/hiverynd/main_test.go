package main

import (
	"bytes"
	"strings"
	"testing"
)

// An unknown command — including the removed `action` CLI — is a usage error,
// never a server start.
func TestUnknownCommandIsUsageError(t *testing.T) {
	for _, args := range [][]string{{"frobnicate"}, {"action", "validate", "."}} {
		var out, errOut bytes.Buffer
		code := run(args, &out, &errOut)
		if code != exitUsage || out.Len() != 0 || !strings.Contains(errOut.String(), "unknown command") {
			t.Errorf("%v: code %d stdout %q stderr %q", args, code, out.String(), errOut.String())
		}
	}
}
