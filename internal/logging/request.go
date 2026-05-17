package logging

import "time"

type RequestLogger struct {
	file *lineFile
}

type RequestEntry struct {
	Timestamp   string `json:"ts"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Status      int    `json:"status"`
	DurationMS  int64  `json:"duration_ms"`
	RequestBody any    `json:"req_body,omitempty"`
	Response    any    `json:"res_envelope,omitempty"`
	Context     string `json:"ctx,omitempty"`
}

func (l *RequestLogger) Log(entry RequestEntry) {
	if entry.Timestamp == "" {
		entry.Timestamp = formatTimestamp(time.Now())
	}
	writeJSONLine(l.file, entry)
}
