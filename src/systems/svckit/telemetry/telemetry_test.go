package telemetry

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// recordingHandler captures Handle calls and tracks Enabled queries.
type recordingHandler struct {
	enabled bool
	records []slog.Record
	attrs   []slog.Attr
	group   string
}

func (h *recordingHandler) Enabled(_ context.Context, _ slog.Level) bool { return h.enabled }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r)
	return nil
}
func (h *recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &recordingHandler{enabled: h.enabled, attrs: attrs}
}
func (h *recordingHandler) WithGroup(name string) slog.Handler {
	return &recordingHandler{enabled: h.enabled, group: name}
}

func TestNewFanoutHandlerEmpty(t *testing.T) {
	fh := NewFanoutHandler()
	if fh == nil {
		t.Fatal("expected non-nil FanoutHandler")
	}
}

func TestFanoutHandlerEnabledNoneEnabled(t *testing.T) {
	a := &recordingHandler{enabled: false}
	b := &recordingHandler{enabled: false}
	fh := NewFanoutHandler(a, b)
	if fh.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Enabled should return false when no handler is enabled")
	}
}

func TestFanoutHandlerEnabledOneEnabled(t *testing.T) {
	a := &recordingHandler{enabled: false}
	b := &recordingHandler{enabled: true}
	fh := NewFanoutHandler(a, b)
	if !fh.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Enabled should return true when at least one handler is enabled")
	}
}

func TestFanoutHandlerHandle(t *testing.T) {
	a := &recordingHandler{enabled: true}
	b := &recordingHandler{enabled: true}
	c := &recordingHandler{enabled: false}
	fh := NewFanoutHandler(a, b, c)

	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "test message", 0)
	if err := fh.Handle(context.Background(), rec); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(a.records) != 1 {
		t.Errorf("handler a: got %d records, want 1", len(a.records))
	}
	if len(b.records) != 1 {
		t.Errorf("handler b: got %d records, want 1", len(b.records))
	}
	if len(c.records) != 0 {
		t.Errorf("handler c (disabled): got %d records, want 0", len(c.records))
	}
}

func TestFanoutHandlerWithAttrs(t *testing.T) {
	a := &recordingHandler{enabled: true}
	b := &recordingHandler{enabled: true}
	fh := NewFanoutHandler(a, b)

	attrs := []slog.Attr{slog.String("key", "val")}
	result := fh.WithAttrs(attrs)
	fh2, ok := result.(*FanoutHandler)
	if !ok {
		t.Fatalf("WithAttrs returned %T, want *FanoutHandler", result)
	}
	if len(fh2.handlers) != 2 {
		t.Errorf("got %d handlers, want 2", len(fh2.handlers))
	}
	for i, h := range fh2.handlers {
		rh, ok := h.(*recordingHandler)
		if !ok {
			t.Errorf("handler[%d] is %T, want *recordingHandler", i, h)
			continue
		}
		if len(rh.attrs) != 1 || rh.attrs[0].Key != "key" {
			t.Errorf("handler[%d] attrs not propagated", i)
		}
	}
}

func TestFanoutHandlerWithGroup(t *testing.T) {
	a := &recordingHandler{enabled: true}
	fh := NewFanoutHandler(a)

	result := fh.WithGroup("mygroup")
	fh2, ok := result.(*FanoutHandler)
	if !ok {
		t.Fatalf("WithGroup returned %T, want *FanoutHandler", result)
	}
	rh, ok := fh2.handlers[0].(*recordingHandler)
	if !ok {
		t.Fatalf("handler[0] is %T, want *recordingHandler", fh2.handlers[0])
	}
	if rh.group != "mygroup" {
		t.Errorf("group = %q, want %q", rh.group, "mygroup")
	}
}

func TestSetupErrorWhenNoEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	_, _, err := Setup(context.Background(), "testsvc")
	if err == nil {
		t.Error("expected error when OTEL_EXPORTER_OTLP_ENDPOINT is not set")
	}
}

func TestSetupUsesServiceNameEnv(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "custom-name")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	_, _, err := Setup(context.Background(), "default-name")
	if err == nil {
		t.Error("expected error when OTEL_EXPORTER_OTLP_ENDPOINT is not set")
	}
}
