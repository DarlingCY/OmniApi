package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/omniapi/omni-api/internal/model"
)

type failedStreamReader struct{}

func (failedStreamReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestStreamReadFailureIsNotSuccessfulCompletion(t *testing.T) {
	response := &http.Response{Body: io.NopCloser(io.MultiReader(
		strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"), failedStreamReader{}))}
	stream, err := newStream(model.ProtocolOpenAIChat, response)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if text, ok := stream.Next(); !ok || text != "hello" {
		t.Fatal("missing first delta")
	}
	if _, ok := stream.Next(); ok {
		t.Fatal("unexpected next delta")
	}
	if !errors.Is(stream.Err(), io.ErrUnexpectedEOF) {
		t.Fatalf("lost underlying read error: %v", stream.Err())
	}
	if !strings.Contains(Diagnostic(stream.Err()), "unexpected EOF") {
		t.Fatal("missing diagnostic cause")
	}
}

func TestStreamTerminalEventStopsReadingConnection(t *testing.T) {
	for _, terminal := range []string{"[DONE]", `{"type":"response.completed"}`, `{"type":"message_stop"}`} {
		response := &http.Response{Body: io.NopCloser(io.MultiReader(strings.NewReader(
			"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: "+terminal+"\n\n"), failedStreamReader{}))}
		stream, err := newStream(model.ProtocolOpenAIChat, response)
		if err != nil {
			t.Fatal(err)
		}
		stream.Next()
		if _, ok := stream.Next(); ok || stream.Err() != nil {
			t.Fatalf("terminal event should finish without reading further: %v", stream.Err())
		}
		stream.Close()
	}
}

func TestUpstreamTimeoutKeepsDiagnosticCause(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer up.Close()
	provider := anthropicProvider(up.URL)
	_, err := NewClient(50*time.Millisecond).Call(context.Background(), provider, provider.Models[0], model.Request{Model: "demo"})
	var failure *Failure
	if !errors.As(err, &failure) || failure.Status != 504 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected timeout with cause, got %v", err)
	}
	if strings.Contains(Diagnostic(err), up.URL) {
		t.Fatal("network diagnostic retained full request URL")
	}
}
