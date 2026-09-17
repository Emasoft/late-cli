package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStatusError_ErrorFormat(t *testing.T) {
	tests := []struct {
		name string
		se   *StatusError
		want string
	}{
		{
			name: "with body renders legacy API error format",
			se:   &StatusError{StatusCode: 500, Status: "500 Internal Server Error", Body: "internal error"},
			want: "API error (500): internal error",
		},
		{
			name: "without body renders legacy status format",
			se:   &StatusError{StatusCode: 429, Status: "429 Too Many Requests"},
			want: "status: 429",
		},
		{
			name: "empty body renders legacy status format",
			se:   &StatusError{StatusCode: 408, Status: "408 Request Timeout", Body: ""},
			want: "status: 408",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.se.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStatusError_ErrorsAsThroughWrapChain(t *testing.T) {
	se := &StatusError{StatusCode: 503, Status: "503 Service Unavailable", Body: "overloaded"}

	// Mirrors the real chains: executor.go wraps with "stream error: %w" and
	// client.go with "stream interrupted: %w".
	wrapped := fmt.Errorf("a: %w", fmt.Errorf("stream error: %w", se))

	var got *StatusError
	if !errors.As(wrapped, &got) {
		t.Fatal("errors.As failed to recover *StatusError through wrap chain")
	}
	if got.StatusCode != 503 {
		t.Errorf("StatusCode = %d, want 503", got.StatusCode)
	}
	if got != se {
		t.Error("errors.As recovered a different *StatusError instance")
	}

	// The legacy message text must survive wrapping unchanged.
	want := "a: stream error: API error (503): overloaded"
	if msg := wrapped.Error(); msg != want {
		t.Errorf("wrapped message = %q, want %q", msg, want)
	}
}

func TestFormatError_ReturnsTypedStatusError(t *testing.T) {
	t.Run("API error body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":{"message":"boom","type":"invalid_request_error","code":"invalid_api_key"}}`)
		}))
		defer server.Close()

		c := NewClient(Config{BaseURL: server.URL})
		_, err := c.ChatCompletion(context.Background(), ChatCompletionRequest{
			Model:    "test-model",
			Messages: []ChatMessage{{Role: "user", Content: TextContent("hi")}},
		})
		if err == nil {
			t.Fatal("expected error for 500 response, got nil")
		}

		var se *StatusError
		if !errors.As(err, &se) {
			t.Fatalf("error %T (%v) is not a *StatusError", err, err)
		}
		if se.StatusCode != http.StatusInternalServerError {
			t.Errorf("StatusCode = %d, want %d", se.StatusCode, http.StatusInternalServerError)
		}
		if want := "API error (500): boom"; se.Error() != want {
			t.Errorf("Error() = %q, want %q", se.Error(), want)
		}
		if se.Type != "invalid_request_error" {
			t.Errorf("Type = %q, want %q", se.Type, "invalid_request_error")
		}
		if se.Code != "invalid_api_key" {
			t.Errorf("Code = %v, want %q", se.Code, "invalid_api_key")
		}
	})

	t.Run("body without type or code fields", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"rate limited"}}`)
		}))
		defer server.Close()

		c := NewClient(Config{BaseURL: server.URL})
		_, err := c.ChatCompletion(context.Background(), ChatCompletionRequest{
			Model:    "test-model",
			Messages: []ChatMessage{{Role: "user", Content: TextContent("hi")}},
		})
		if err == nil {
			t.Fatal("expected error for 429 response, got nil")
		}

		var se *StatusError
		if !errors.As(err, &se) {
			t.Fatalf("error %T (%v) is not a *StatusError", err, err)
		}
		if se.StatusCode != http.StatusTooManyRequests {
			t.Errorf("StatusCode = %d, want %d", se.StatusCode, http.StatusTooManyRequests)
		}
		if se.Type != "" {
			t.Errorf("Type = %q, want empty", se.Type)
		}
		if se.Code != nil {
			t.Errorf("Code = %v, want nil", se.Code)
		}
		if want := "rate limited"; se.Body != want {
			t.Errorf("Body = %q, want %q", se.Body, want)
		}
		if want := "API error (429): rate limited"; se.Error() != want {
			t.Errorf("Error() = %q, want %q", se.Error(), want)
		}
	})

	t.Run("unparseable body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, "not json")
		}))
		defer server.Close()

		c := NewClient(Config{BaseURL: server.URL})
		_, err := c.ChatCompletion(context.Background(), ChatCompletionRequest{
			Model:    "test-model",
			Messages: []ChatMessage{{Role: "user", Content: TextContent("hi")}},
		})
		if err == nil {
			t.Fatal("expected error for 502 response, got nil")
		}

		var se *StatusError
		if !errors.As(err, &se) {
			t.Fatalf("error %T (%v) is not a *StatusError", err, err)
		}
		if se.StatusCode != http.StatusBadGateway {
			t.Errorf("StatusCode = %d, want %d", se.StatusCode, http.StatusBadGateway)
		}
		if want := "status: 502"; se.Error() != want {
			t.Errorf("Error() = %q, want %q", se.Error(), want)
		}
	})
}
