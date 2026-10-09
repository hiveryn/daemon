package remoteexec

import (
	"bytes"
	"context"
	"fmt"
	"os"
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
	if f.TempDir != "" {
		temp = Quote(f.TempDir + "/" + prefix + ".XXXXXX")
	}
	out, err := f.run("umask 077; p=$(mktemp "+temp+") || exit; if cat >\"$p\"; then printf '%s' \"$p\"; else rm -f \"$p\"; exit 1; fi", b)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
func NewFileSystem(ctx context.Context, alias string) (*FileSystem, error) {
	out, err := Run(ctx, alias, `printf '%s\n' "$HOME" "$CODEX_HOME" "$CLAUDE_CONFIG_DIR" "$XDG_CONFIG_HOME" "$SHELL"`, nil)
	if err != nil {
		return nil, err
	}
	values := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(values) != 5 {
		return nil, fmt.Errorf("SSH %s returned invalid environment response", alias)
	}
	env := map[string]string{}
	for i, k := range []string{"HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "XDG_CONFIG_HOME", "SHELL"} {
		env[k] = values[i]
	}
	return &FileSystem{Context: ctx, Alias: alias, Env: env}, nil
}
