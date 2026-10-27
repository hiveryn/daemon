package remoteexec

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// FileSystem prepares provider files on the target. Env contains only explicitly
// queried target configuration directories; never the laptop environment.
type FileSystem struct {
	Context context.Context
	Alias   string
	Env     map[string]string
	TempDir string
}

func (f *FileSystem) run(script string, data []byte) ([]byte, error) {
	return Run(f.Context, f.Alias, script, bytes.NewReader(data))
}
func (f *FileSystem) ReadFile(p string) ([]byte, error) {
	b, err := f.run("if test -e "+Quote(p)+"; then printf '1'; cat "+Quote(p)+"; else printf '0'; fi", nil)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("empty SSH file response")
	}
	if b[0] == '0' {
		return nil, os.ErrNotExist
	}
	return b[1:], nil
}
func (f *FileSystem) WriteFile(p string, b []byte, m os.FileMode) error {
	// Use a sibling temp and rename so a failed transfer cannot truncate config.
	_, err := f.run("umask 077; p="+Quote(p)+"; tmp=$(mktemp \"${p}.XXXXXX\") || exit; trap 'rm -f \"$tmp\"' EXIT; cat >\"$tmp\" && chmod "+fmt.Sprintf("%o", m.Perm())+" \"$tmp\" && mv -f \"$tmp\" \"$p\"", b)
	return err
}
func (f *FileSystem) MkdirAll(p string, m os.FileMode) error {
	_, err := f.run("mkdir -p -m "+fmt.Sprintf("%o", m.Perm())+" "+Quote(p), nil)
	return err
}
func (f *FileSystem) Remove(p string) error { _, err := f.run("rm -f "+Quote(p), nil); return err }
func (f *FileSystem) UserHomeDir() (string, error) {
	if h := f.Env["HOME"]; h != "" {
		return h, nil
	}
	return "", fmt.Errorf("remote HOME is unavailable")
}
func (f *FileSystem) Getenv(k string) string { return f.Env[k] }
func (f *FileSystem) WriteTemp(pattern string, b []byte) (string, error) {
	prefix := strings.ReplaceAll(pattern, "*", "")
	temp := `"${TMPDIR:-/tmp}/` + prefix + `.XXXXXX"`
	mkdir := ""
	if f.TempDir != "" {
		// The private directory is created by its first file, saving a round trip.
		temp = Quote(f.TempDir + "/" + prefix + ".XXXXXX")
		mkdir = "mkdir -p -m 700 " + Quote(f.TempDir) + " || exit; "
	}
	out, err := f.run(mkdir+"umask 077; p=$(mktemp "+temp+") || exit; if cat >\"$p\"; then printf '%s' \"$p\"; else rm -f \"$p\"; exit 1; fi", b)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// MissingCommandsError names the required commands a target's noninteractive
// shell could not find.
type MissingCommandsError struct {
	Alias    string
	Commands []string
}

func (e *MissingCommandsError) Error() string {
	return fmt.Sprintf("remote prerequisite %s is unavailable on %s: not found by command -v in a noninteractive SSH shell", strings.Join(e.Commands, ", "), e.Alias)
}

// envKeys are the target configuration directories a FileSystem knows.
var envKeys = []string{"HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "XDG_CONFIG_HOME", "SHELL"}

// NewFileSystem queries the target's environment and, in the same command,
// confirms each required command is available (a *MissingCommandsError names
// every one that is not).
func NewFileSystem(ctx context.Context, alias string, required ...string) (*FileSystem, error) {
	var script strings.Builder
	script.WriteString("missing=\n")
	for i, command := range required {
		fmt.Fprintf(&script, "command -v %s >/dev/null 2>&1 || missing=\"$missing %d\"\n", Quote(command), i)
	}
	script.WriteString(`printf '%s\n' "$missing" "$HOME" "$CODEX_HOME" "$CLAUDE_CONFIG_DIR" "$XDG_CONFIG_HOME" "$SHELL"`)
	out, err := Run(ctx, alias, script.String(), nil)
	if err != nil {
		return nil, err
	}
	values := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(values) != len(envKeys)+1 {
		return nil, fmt.Errorf("SSH %s returned invalid environment response", alias)
	}
	if missing := strings.Fields(values[0]); len(missing) > 0 {
		e := &MissingCommandsError{Alias: alias}
		for _, index := range missing {
			i, err := strconv.Atoi(index)
			if err != nil || i < 0 || i >= len(required) {
				return nil, fmt.Errorf("SSH %s returned invalid prerequisite response", alias)
			}
			e.Commands = append(e.Commands, required[i])
		}
		return nil, e
	}
	env := map[string]string{}
	for i, k := range envKeys {
		env[k] = values[i+1]
	}
	return &FileSystem{Context: ctx, Alias: alias, Env: env}, nil
}
