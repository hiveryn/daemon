package logging

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

type appHandler struct {
	level  *slog.LevelVar
	file   *lineFile
	attrs  []flatAttr
	groups []string
}

type flatAttr struct {
	key   string
	value slog.Value
}

type appEntry struct {
	Timestamp string     `json:"ts"`
	Level     string     `json:"lvl"`
	Source    string     `json:"src"`
	Message   string     `json:"msg"`
	File      string     `json:"file"`
	Line      int        `json:"line"`
	Function  string     `json:"fn"`
	Error     *errorBody `json:"err,omitempty"`
	Context   string     `json:"ctx,omitempty"`
	Body      any        `json:"body,omitempty"`
}

type errorBody struct {
	Message string `json:"message"`
	Stack   string `json:"stack"`
}

func (h *appHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *appHandler) Handle(_ context.Context, record slog.Record) error {
	collector := attrCollector{}
	for _, attr := range h.attrs {
		collector.add(attr.key, attr.value)
	}
	record.Attrs(func(attr slog.Attr) bool {
		collector.addAttr(h.groups, attr)
		return true
	})

	file, line, fn := sourceFields(record.PC)
	entry := appEntry{
		Timestamp: formatTimestamp(record.Time),
		Level:     levelName(record.Level),
		Source:    "daemon",
		Message:   record.Message,
		File:      file,
		Line:      line,
		Function:  fn,
		Error:     collector.err,
		Context:   collector.context(),
		Body:      collector.body,
	}
	writeJSONLine(h.file, entry)
	return nil
}

func (h *appHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := &appHandler{
		level:  h.level,
		file:   h.file,
		attrs:  append([]flatAttr(nil), h.attrs...),
		groups: append([]string(nil), h.groups...),
	}
	for _, attr := range attrs {
		clone.attrs = append(clone.attrs, flattenAttr(h.groups, attr)...)
	}
	return clone
}

func (h *appHandler) WithGroup(name string) slog.Handler {
	clone := &appHandler{
		level:  h.level,
		file:   h.file,
		attrs:  append([]flatAttr(nil), h.attrs...),
		groups: append([]string(nil), h.groups...),
	}
	if name != "" {
		clone.groups = append(clone.groups, name)
	}
	return clone
}

func flattenAttr(groups []string, attr slog.Attr) []flatAttr {
	attr.Value = attr.Value.Resolve()
	if attr.Equal(slog.Attr{}) {
		return nil
	}
	if attr.Value.Kind() == slog.KindGroup {
		nextGroups := append(append([]string(nil), groups...), attr.Key)
		flat := make([]flatAttr, 0, len(attr.Value.Group()))
		for _, item := range attr.Value.Group() {
			flat = append(flat, flattenAttr(nextGroups, item)...)
		}
		return flat
	}
	return []flatAttr{{key: joinKey(groups, attr.Key), value: attr.Value}}
}

type attrCollector struct {
	err      *errorBody
	ctxParts []string
	body     any
}

func (c *attrCollector) addAttr(groups []string, attr slog.Attr) {
	for _, item := range flattenAttr(groups, attr) {
		c.add(item.key, item.value)
	}
}

func (c *attrCollector) add(key string, value slog.Value) {
	value = value.Resolve()
	if key == "ctx" {
		if ctxValue, ok := scalarContextValue(value); ok {
			c.ctxParts = append(c.ctxParts, trimQuotedContext(ctxValue))
			return
		}
	}
	if key == "body" {
		c.body = structuredValue(value)
		return
	}
	if errBody, ok := valueErrorBody(value); ok {
		c.err = errBody
		return
	}

	if ctxValue, ok := scalarContextValue(value); ok {
		c.ctxParts = append(c.ctxParts, key+"="+ctxValue)
		return
	}

	body := c.ensureBodyMap()
	body[key] = structuredValue(value)
}

func (c *attrCollector) context() string {
	return strings.Join(c.ctxParts, " ")
}

func (c *attrCollector) ensureBodyMap() map[string]any {
	if body, ok := c.body.(map[string]any); ok {
		return body
	}
	body := map[string]any{}
	if c.body != nil {
		body["value"] = c.body
	}
	c.body = body
	return body
}

func valueErrorBody(value slog.Value) (*errorBody, bool) {
	if value.Kind() != slog.KindAny {
		return nil, false
	}
	err, ok := value.Any().(error)
	if !ok || err == nil {
		return nil, false
	}
	return &errorBody{Message: err.Error(), Stack: string(debug.Stack())}, true
}

func scalarContextValue(value slog.Value) (string, bool) {
	switch value.Kind() {
	case slog.KindBool:
		return strconv.FormatBool(value.Bool()), true
	case slog.KindDuration:
		return value.Duration().String(), true
	case slog.KindFloat64:
		return strconv.FormatFloat(value.Float64(), 'f', -1, 64), true
	case slog.KindInt64:
		return strconv.FormatInt(value.Int64(), 10), true
	case slog.KindString:
		return quoteIfNeeded(value.String()), true
	case slog.KindTime:
		return formatTimestamp(value.Time()), true
	case slog.KindUint64:
		return strconv.FormatUint(value.Uint64(), 10), true
	case slog.KindAny:
		switch v := value.Any().(type) {
		case nil:
			return "null", true
		case fmt.Stringer:
			return quoteIfNeeded(v.String()), true
		case []byte:
			return quoteIfNeeded(string(v)), true
		}
	}
	return "", false
}

func structuredValue(value slog.Value) any {
	switch value.Kind() {
	case slog.KindBool:
		return value.Bool()
	case slog.KindDuration:
		return value.Duration().String()
	case slog.KindFloat64:
		return value.Float64()
	case slog.KindInt64:
		return value.Int64()
	case slog.KindString:
		return value.String()
	case slog.KindTime:
		return formatTimestamp(value.Time())
	case slog.KindUint64:
		return value.Uint64()
	case slog.KindGroup:
		group := map[string]any{}
		for _, attr := range value.Group() {
			attr.Value = attr.Value.Resolve()
			group[attr.Key] = structuredValue(attr.Value)
		}
		return group
	case slog.KindAny:
		return value.Any()
	default:
		return value.Any()
	}
}

func sourceFields(pc uintptr) (string, int, string) {
	frames := runtime.CallersFrames([]uintptr{pc})
	frame, _ := frames.Next()
	if frame.File == "" {
		return "", 0, ""
	}
	return filepath.Base(frame.File), frame.Line, shortFunctionName(frame.Function)
}

func levelName(level slog.Level) string {
	if level <= slog.LevelDebug {
		return "debug"
	}
	if level < slog.LevelWarn {
		return "info"
	}
	if level < slog.LevelError {
		return "warn"
	}
	return "error"
}

func formatTimestamp(ts time.Time) string {
	if ts.IsZero() {
		ts = time.Now()
	}
	return ts.UTC().Format(timestampLayout)
}

func joinKey(groups []string, key string) string {
	parts := append(append([]string(nil), groups...), key)
	return strings.Join(parts, ".")
}

func quoteIfNeeded(value string) string {
	if strings.ContainsAny(value, " \t\n\r\"") {
		return strconv.Quote(value)
	}
	return value
}

func trimQuotedContext(value string) string {
	if unquoted, err := strconv.Unquote(value); err == nil {
		return unquoted
	}
	return value
}

func shortFunctionName(fn string) string {
	fn = filepath.Base(fn)
	if idx := strings.LastIndexByte(fn, '.'); idx >= 0 && idx+1 < len(fn) {
		return fn[idx+1:]
	}
	return fn
}
