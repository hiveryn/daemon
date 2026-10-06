package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// NotificationsConfig is config.yaml's optional notifications section. It is
// part of the daemon core, read at startup like port and log_level.
type NotificationsConfig struct {
	Ntfy *NtfyConfig `yaml:"ntfy,omitempty"`
}

// NtfyConfig is where agent notifications and questions are published. Server
// and Topic are required; authentication is optional and is either an access
// Token or a Username/Password pair. Credentials are never logged or echoed in
// validation or delivery errors.
type NtfyConfig struct {
	Server   string `yaml:"server"`
	Topic    string `yaml:"topic"`
	Token    string `yaml:"token,omitempty"`
	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`
}

// ntfyTopicPattern is ntfy's own topic rule.
var ntfyTopicPattern = regexp.MustCompile(`^[-_A-Za-z0-9]{1,64}$`)

func (n *NotificationsConfig) clone() *NotificationsConfig {
	if n == nil {
		return nil
	}
	out := &NotificationsConfig{}
	if n.Ntfy != nil {
		ntfy := *n.Ntfy
		out.Ntfy = &ntfy
	}
	return out
}

func (n *NotificationsConfig) normalize() {
	if n == nil || n.Ntfy == nil {
		return
	}
	n.Ntfy.Server = strings.TrimRight(strings.TrimSpace(n.Ntfy.Server), "/")
	n.Ntfy.Topic = strings.TrimSpace(n.Ntfy.Topic)
	n.Ntfy.Token = strings.TrimSpace(n.Ntfy.Token)
	n.Ntfy.Username = strings.TrimSpace(n.Ntfy.Username)
}

// validate reports configuration mistakes by field. Messages name fields and
// never include the token or password values.
func (n *NotificationsConfig) validate() error {
	if n == nil || n.Ntfy == nil {
		return nil
	}
	ntfy := n.Ntfy
	if ntfy.Server == "" {
		return fmt.Errorf("notifications.ntfy.server is required (for example https://ntfy.sh)")
	}
	// The server value is not echoed: a malformed one may embed credentials
	// (userinfo, or ntfy's ?auth= query parameter).
	u, err := url.Parse(ntfy.Server)
	if err != nil {
		return fmt.Errorf("notifications.ntfy.server is not a valid URL")
	}
	if u.User != nil {
		return fmt.Errorf("notifications.ntfy.server must not embed credentials; use notifications.ntfy.token or username/password")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("notifications.ntfy.server must not have a query or fragment; use notifications.ntfy.token or username/password for authentication")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("notifications.ntfy.server must be an absolute http(s) URL such as https://ntfy.sh")
	}
	if ntfy.Topic == "" {
		return fmt.Errorf("notifications.ntfy.topic is required")
	}
	if !ntfyTopicPattern.MatchString(ntfy.Topic) {
		return fmt.Errorf("notifications.ntfy.topic must be 1-64 letters, digits, '-' or '_'")
	}
	hasBasic := ntfy.Username != "" || ntfy.Password != ""
	if ntfy.Token != "" && hasBasic {
		return fmt.Errorf("notifications.ntfy: set either token or username/password, not both")
	}
	if hasBasic && (ntfy.Username == "" || ntfy.Password == "") {
		return fmt.Errorf("notifications.ntfy: username and password must be set together")
	}
	return nil
}
