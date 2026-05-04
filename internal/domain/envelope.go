package domain

type LogEntry struct {
	Level     string `json:"level"`
	Message   string `json:"message"`
	Timestamp string `json:"timestamp"`
}

type Meta struct {
	RequestID string `json:"request_id"`
}

type ErrorBody struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Details    any    `json:"details,omitempty"`
	Stacktrace string `json:"stacktrace"`
}

type Envelope struct {
	Data     any        `json:"data"`
	Error    *ErrorBody `json:"error"`
	Logs     []LogEntry `json:"logs"`
	Commands []any      `json:"commands"`
	Meta     Meta       `json:"meta"`
}
