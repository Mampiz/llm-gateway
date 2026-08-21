package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestError_Message(t *testing.T) {
	tests := []struct {
		name  string
		err   *Error
		parts []string
	}{
		{
			name:  "with a status code",
			err:   &Error{Provider: "openai", StatusCode: 429, Message: "quota exceeded"},
			parts: []string{"openai", "429", "quota exceeded"},
		},
		{
			name:  "without one",
			err:   &Error{Provider: "anthropic", Message: "connection refused"},
			parts: []string{"anthropic", "connection refused"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.err.Error()
			for _, part := range tt.parts {
				if !strings.Contains(got, part) {
					t.Errorf("Error() = %q, want it to contain %q", got, part)
				}
			}
		})
	}
}

// TestError_Unwrap is the assertion that keeps errors.Is working through our
// own error type. Without Unwrap, a cancellation wrapped by a provider becomes
// invisible and the HTTP layer misclassifies it.
func TestError_Unwrap(t *testing.T) {
	cause := errors.New("underlying failure")
	err := &Error{Provider: "openai", Message: "wrapped", Err: cause}

	if !errors.Is(err, cause) {
		t.Error("errors.Is could not find the cause through Unwrap")
	}
	if errors.Is(err, context.Canceled) {
		t.Error("errors.Is matched an unrelated target")
	}

	// A nil cause must not blow up or match anything.
	if errors.Is(&Error{Message: "no cause"}, cause) {
		t.Error("an Error with no cause matched a target")
	}
}

func TestError_Retryable(t *testing.T) {
	tests := []struct {
		name string
		err  *Error
		want bool
	}{
		{"transport failure", &Error{StatusCode: 0}, true},
		{"rate limited", &Error{StatusCode: 429}, true},
		{"server error", &Error{StatusCode: 500}, true},
		{"bad gateway", &Error{StatusCode: 502}, true},
		{"bad request", &Error{StatusCode: 400}, false},
		{"unauthorized", &Error{StatusCode: 401}, false},
		{"not found", &Error{StatusCode: 404}, false},
		// Nobody is left waiting for the answer, so a retry helps no one even
		// though the status code alone would suggest otherwise.
		{"cancelled by the caller", &Error{StatusCode: 0, Err: context.Canceled}, false},
		{"deadline exceeded", &Error{StatusCode: 0, Err: context.DeadlineExceeded}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Retryable(); got != tt.want {
				t.Errorf("Retryable() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestError_AsTarget documents how callers are meant to inspect our errors.
func TestError_AsTarget(t *testing.T) {
	var err error = &Error{Provider: "openai", StatusCode: 503, Message: "overloaded"}

	var pErr *Error
	if !errors.As(err, &pErr) {
		t.Fatal("errors.As failed to extract a *Error")
	}
	if pErr.StatusCode != 503 {
		t.Errorf("StatusCode = %d, want 503", pErr.StatusCode)
	}
}
