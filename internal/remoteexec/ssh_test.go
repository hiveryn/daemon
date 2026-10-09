package remoteexec

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestShellArgumentsAreLiteral(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"plain", "a'b", "$(echo unsafe)", "`echo unsafe`", "a\nb", "; exit 12", "-n", ""} {
		out, err := exec.Command("sh", "-c", "printf '%s' "+Quote(value)).Output()
		if err != nil || string(out) != value {
			t.Fatalf("%q => %q, %v", value, out, err)
		}
	}
}

func TestRunNamesTheCancellationThatKilledSSH(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, "unused-alias", "true", nil)
	if !errors.Is(err, context.Canceled) || !strings.HasPrefix(err.Error(), "SSH unused-alias: context canceled") {
		t.Fatalf("cancelled run: %v", err)
	}
}
