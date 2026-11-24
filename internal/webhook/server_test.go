package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AugustRoad/minecraft-bot/internal/events"
)

func TestHandleEvents_AuthAndValidation(t *testing.T) {
	var received []events.Event
	srv := NewServer(":0", "super-secret", func(ctx context.Context, ev events.Event) {
		received = append(received, ev)
	}, nil)

	handler := srv.httpServer.Handler

	// 1. Missing/wrong secret -> 401
	req := httptest.NewRequest(http.MethodPost, "/events", bytes.NewBufferString(`{"type":"vm_boot"}`))
	req.Header.Set(HeaderSecret, "wrong-secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong secret, got %d", rec.Code)
	}

	// 2. Unknown event type -> 400
	req = httptest.NewRequest(http.MethodPost, "/events", bytes.NewBufferString(`{"type":"not_real"}`))
	req.Header.Set(HeaderSecret, "super-secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid event type, got %d", rec.Code)
	}

	// 3. Valid event -> 202 and delivered to handler
	payload, _ := json.Marshal(events.Event{
		Type:    events.TypeChat,
		Player:  "Steve",
		Message: "hi!",
	})
	req = httptest.NewRequest(http.MethodPost, "/events", bytes.NewReader(payload))
	req.Header.Set(HeaderSecret, "super-secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 for valid event, got %d", rec.Code)
	}
	if len(received) != 1 || received[0].Player != "Steve" || received[0].Message != "hi!" {
		t.Fatalf("unexpected received events: %+v", received)
	}

	// 4. Health check -> 200
	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /healthz, got %d", rec.Code)
	}
}
