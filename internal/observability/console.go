package observability

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
)

// consoleOutput serializes complete records across handlers derived with With.
type consoleOutput struct {
	mu     sync.Mutex
	writer io.Writer
	fields bytes.Buffer
}

type consoleHandler struct {
	output  *consoleOutput
	encoder slog.Handler
}

// NewTextLogger renders the level, message, and standard key=value attributes.
// Each record occupies one physical line, including records with multiline input.
func NewTextLogger(writer io.Writer, debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	output := &consoleOutput{writer: writer}
	encoder := slog.NewTextHandler(&output.fields, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			if len(groups) == 0 && (attr.Key == slog.TimeKey || attr.Key == slog.LevelKey || attr.Key == slog.MessageKey) {
				return slog.Attr{}
			}
			if attr.Value.Kind() == slog.KindString {
				attr.Value = slog.StringValue(singleLogLine(attr.Value.String()))
			} else if err, ok := attr.Value.Any().(error); ok {
				attr.Value = slog.StringValue(singleLogLine(err.Error()))
			}
			return attr
		},
	})
	return slog.New(&consoleHandler{output: output, encoder: encoder})
}

func singleLogLine(text string) string {
	if strings.ContainsAny(text, "\r\n\t") {
		return strings.Join(strings.Fields(text), " ")
	}
	return strings.TrimSpace(text)
}

func (handler *consoleHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return handler.encoder.Enabled(ctx, level)
}

func (handler *consoleHandler) Handle(ctx context.Context, record slog.Record) error {
	output := handler.output
	output.mu.Lock()
	defer output.mu.Unlock()
	output.fields.Reset()
	if err := handler.encoder.Handle(ctx, record); err != nil {
		return err
	}
	var line bytes.Buffer
	fmt.Fprintf(&line, "%s %s", record.Level.String(), singleLogLine(record.Message))
	fields := bytes.TrimSuffix(output.fields.Bytes(), []byte("\n"))
	if len(fields) > 0 {
		line.WriteByte(' ')
		line.Write(fields)
	}
	line.WriteByte('\n')
	_, err := output.writer.Write(line.Bytes())
	return err
}

func (handler *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &consoleHandler{output: handler.output, encoder: handler.encoder.WithAttrs(attrs)}
}

func (handler *consoleHandler) WithGroup(name string) slog.Handler {
	return &consoleHandler{output: handler.output, encoder: handler.encoder.WithGroup(name)}
}
