package lumber

import (
	"context"
	"log/slog"
	"runtime"
	"slices"
	"time"
)

// LevelFatal is the slog level Lumber sends as FATAL: logger.Log(ctx, lumber.LevelFatal, "db down"). Any level from
// slog.LevelError up to it is ERROR.
const LevelFatal = slog.Level(12)

// HandlerOptions configure Client.Handler.
type HandlerOptions struct {
	// Level is the lowest level sent. Nil means slog.LevelInfo.
	Level slog.Leveler
	// AddSource sends the file, line and function that logged in each log's context.
	AddSource bool
}

// Handler is a slog.Handler that sends records to Lumber:
//
//	logger := slog.New(client.Handler(nil))
//	logger.Info("Server started", "port", 8080)
//
// Attributes go into the log's metadata, groups as nested objects, except three at the top level: request_id and
// trace_id (or requestId and traceId) are the log's own fields, and a "context" group or map goes into its context.
// An error value is sent as its message.
func (c *Client) Handler(opts *HandlerOptions) slog.Handler {
	h := &handler{client: c, level: slog.LevelInfo}
	if opts != nil {
		if opts.Level != nil {
			h.level = opts.Level
		}
		h.addSource = opts.AddSource
	}
	return h
}

type handler struct {
	client    *Client
	level     slog.Leveler
	addSource bool
	attrs     map[string]any // from WithAttrs, nested under their groups
	groups    []string
}

func (h *handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *handler) Handle(_ context.Context, record slog.Record) error {
	data := deepCopy(h.attrs)
	record.Attrs(func(attr slog.Attr) bool {
		data = addAttr(data, h.groups, attr)
		return true
	})
	var context map[string]any
	if h.addSource && record.PC != 0 {
		frame, _ := runtime.CallersFrames([]uintptr{record.PC}).Next()
		context = map[string]any{"code": frame.File, "line": frame.Line, "method": frame.Function}
	}
	h.client.send(wireLevel(record.Level), record.Message, data, context)
	return nil
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	next := *h
	next.attrs = deepCopy(h.attrs)
	for _, attr := range attrs {
		next.attrs = addAttr(next.attrs, h.groups, attr)
	}
	return &next
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := *h
	next.groups = append(slices.Clip(h.groups), name)
	return &next
}

// wireLevel is a slog level as Lumber's: from LevelFatal up is FATAL, and a custom level counts as the standard one
// below it.
func wireLevel(level slog.Level) int {
	switch {
	case level >= LevelFatal:
		return wireFatal
	case level >= slog.LevelError:
		return wireError
	case level >= slog.LevelWarn:
		return wireWarn
	case level >= slog.LevelInfo:
		return wireInfo
	default:
		return wireDebug
	}
}

// addAttr puts an attribute into data under its groups, following slog's rules: an empty attribute or group is left
// out, and a group with no key is inlined.
func addAttr(data map[string]any, groups []string, attr slog.Attr) map[string]any {
	attr.Value = attr.Value.Resolve()
	if attr.Equal(slog.Attr{}) {
		return data
	}
	if attr.Value.Kind() == slog.KindGroup {
		inner := attr.Value.Group()
		if len(inner) == 0 {
			return data
		}
		if attr.Key != "" {
			groups = append(slices.Clip(groups), attr.Key)
		}
		for _, a := range inner {
			data = addAttr(data, groups, a)
		}
		return data
	}
	if attr.Key == "" {
		return data
	}
	if data == nil {
		data = map[string]any{}
	}
	target := data
	for _, group := range groups {
		next, ok := target[group].(map[string]any)
		if !ok {
			next = map[string]any{}
			target[group] = next
		}
		target = next
	}
	target[attr.Key] = attrValue(attr.Value)
	return data
}

func attrValue(value slog.Value) any {
	switch value.Kind() {
	case slog.KindString:
		return value.String()
	case slog.KindInt64:
		return value.Int64()
	case slog.KindUint64:
		return value.Uint64()
	case slog.KindFloat64:
		return value.Float64()
	case slog.KindBool:
		return value.Bool()
	case slog.KindDuration:
		return value.Duration().String()
	case slog.KindTime:
		return value.Time().Format(time.RFC3339Nano)
	default:
		return jsonValue(value.Any())
	}
}

func deepCopy(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for key, value := range m {
		if inner, ok := value.(map[string]any); ok {
			value = deepCopy(inner)
		}
		out[key] = value
	}
	return out
}
