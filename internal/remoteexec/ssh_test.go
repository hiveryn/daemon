package remoteexec

import (
	"os/exec"
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
