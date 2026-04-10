package telemost

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestGetConnectionInfoHonorsContextCancellation(t *testing.T) {
	previousBaseURL := apiBaseURL
	previousClient := apiHTTPClient
	apiBaseURL = "https://telemost.test"
	apiHTTPClient = &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		}),
	}
	t.Cleanup(func() {
		apiBaseURL = previousBaseURL
		apiHTTPClient = previousClient
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, err := GetConnectionInfo(ctx, "https://telemost.yandex.ru/j/test-room", "tester")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GetConnectionInfo() error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("GetConnectionInfo() took too long to abort: %v", elapsed)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
