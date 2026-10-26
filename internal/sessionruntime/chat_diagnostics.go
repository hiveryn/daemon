package sessionruntime

import (
	"regexp"
	"strings"
	"sync"
)

// Keep only a bounded tail and redact credentials before a provider failure is
// returned to the caller (and therefore before the HTTP error logger sees it).
type chatDiagnostics struct {
	mu      sync.Mutex
	tail    string
	secrets []string
}

func newChatDiagnostics(env []string) *chatDiagnostics {
	d := &chatDiagnostics{}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if ok && len(value) >= 4 && (strings.Contains(upper, "TOKEN") || strings.Contains(upper, "SECRET") || strings.Contains(upper, "PASSWORD") || strings.Contains(upper, "API_KEY")) {
			d.secrets = append(d.secrets, value)
		}
	}
	return d
}
func (d *chatDiagnostics) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tail += string(p)
	if len(d.tail) > 16<<10 {
		d.tail = d.tail[len(d.tail)-(16<<10):]
	}
	return len(p), nil
}

var chatTokenPattern = regexp.MustCompile(`(?i)(bearer\s+[^\s"']+|sk-[a-z0-9_-]{12,})`)

func (d *chatDiagnostics) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	text := d.tail
	for _, secret := range d.secrets {
		text = strings.ReplaceAll(text, secret, "[redacted]")
	}
	return strings.TrimSpace(chatTokenPattern.ReplaceAllString(text, "[redacted]"))
}
