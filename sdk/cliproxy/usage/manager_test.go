package usage

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestStreamFromContextDefaultsMissingToFalse(t *testing.T) {
	if StreamFromContext(context.Background()) {
		t.Fatalf("StreamFromContext(background) = true, want false")
	}
}

func TestStreamFromContextHonorsExplicitTrue(t *testing.T) {
	ctx := WithStream(context.Background(), true)
	if !StreamFromContext(ctx) {
		t.Fatalf("StreamFromContext(true) = false, want true")
	}
}

func TestRecordStreamField(t *testing.T) {
	record := Record{
		Provider: "openai",
		Model:    "gpt-5.4",
		Stream:   true,
	}
	if !record.Stream {
		t.Fatalf("Record.Stream = false, want true")
	}
}

func TestRecordBaseURLField(t *testing.T) {
	record := Record{
		Provider: "openai",
		Model:    "gpt-5.4",
		BaseURL:  "https://custom-gateway.example.com/v1",
	}
	if record.BaseURL != "https://custom-gateway.example.com/v1" {
		t.Fatalf("Record.BaseURL = %q, want %q", record.BaseURL, "https://custom-gateway.example.com/v1")
	}
}

func TestGenerateEnabledDefaultsNilToTrue(t *testing.T) {
	if !GenerateEnabled(nil) {
		t.Fatalf("GenerateEnabled(nil) = false, want true")
	}
}

func TestGenerateEnabledHonorsExplicitFalse(t *testing.T) {
	if GenerateEnabled(GenerateFlag(false)) {
		t.Fatalf("GenerateEnabled(false) = true, want false")
	}
}

func TestGenerateEnabledHonorsExplicitTrue(t *testing.T) {
	if !GenerateEnabled(GenerateFlag(true)) {
		t.Fatalf("GenerateEnabled(true) = false, want true")
	}
}

func TestGenerateFromContextDefaultsMissingToTrue(t *testing.T) {
	if !GenerateFromContext(context.Background()) {
		t.Fatalf("GenerateFromContext(background) = false, want true")
	}
}

func TestGenerateFromContextHonorsExplicitFalse(t *testing.T) {
	ctx := WithGenerate(context.Background(), false)
	if GenerateFromContext(ctx) {
		t.Fatalf("GenerateFromContext(false) = true, want false")
	}
}

func TestRecordOmittedGenerateIsEnabled(t *testing.T) {
	// Existing callers construct Record without setting Generate.
	// Omission must remain distinguishable from explicit false and default to true.
	record := Record{
		Provider: "openai",
		Model:    "gpt-5.4",
	}
	if record.Generate != nil {
		t.Fatalf("Record.Generate = %v, want nil for omitted field", record.Generate)
	}
	if !GenerateEnabled(record.Generate) {
		t.Fatalf("GenerateEnabled(omitted) = false, want true")
	}
}

type recordingPlugin struct {
	mu      sync.Mutex
	records []Record
	block   chan struct{}
}

func (p *recordingPlugin) HandleUsage(_ context.Context, record Record) {
	if p.block != nil {
		<-p.block
	}
	p.mu.Lock()
	p.records = append(p.records, record)
	p.mu.Unlock()
}

func (p *recordingPlugin) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.records)
}

func TestManagerStopAndWaitDeliversQueuedRecords(t *testing.T) {
	m := NewManager(0)
	plugin := &recordingPlugin{block: make(chan struct{})}
	m.Register(plugin)
	for i := 0; i < 5; i++ {
		m.Publish(context.Background(), Record{Model: "m"})
	}

	waitErr := make(chan error, 1)
	go func() { waitErr <- m.StopAndWait(context.Background()) }()
	close(plugin.block)

	if err := <-waitErr; err != nil {
		t.Fatalf("StopAndWait() error = %v", err)
	}
	if got := plugin.count(); got != 5 {
		t.Fatalf("delivered records = %d, want 5", got)
	}
}

func TestManagerStopAndWaitReturnsWhenContextEnds(t *testing.T) {
	m := NewManager(0)
	plugin := &recordingPlugin{block: make(chan struct{})}
	defer close(plugin.block)
	m.Register(plugin)
	m.Publish(context.Background(), Record{Model: "m"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.StopAndWait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("StopAndWait() error = %v, want context.Canceled", err)
	}
}

func TestManagerStopAndWaitWithoutStart(t *testing.T) {
	m := NewManager(0)
	if err := m.StopAndWait(context.Background()); err != nil {
		t.Fatalf("StopAndWait() error = %v", err)
	}
	m.Publish(context.Background(), Record{Model: "m"})
}
