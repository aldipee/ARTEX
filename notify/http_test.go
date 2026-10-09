package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// These tests cover HTTP-layer error classification in doJSON.
//
// Channel adapters classify platform-specific business codes (DingTalk errcode,
// Feishu code, Telegram ok), while doJSON classifies HTTP status codes. These are
// independent safeguards: misclassifying 503 would skip retries, while retrying
// a 403 would waste three backoff attempts.

func replyServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoJSONClassifiesHTTPStatus(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		permanent bool
	}{
		{"200 success is not an error", 200, false},
		{"429 rate limit is retryable", 429, false},
		{"408 request timeout is retryable", 408, false},
		{"500 server error is retryable", 500, false},
		{"502 gateway error is retryable", 502, false},
		{"503 service unavailable is retryable", 503, false},
		{"400 bad request is permanent", 400, true},
		{"401 unauthorized is permanent", 401, true},
		{"403 forbidden is permanent", 403, true},
		{"404 not found is permanent", 404, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := replyServer(t, tc.status, `{"detail":"upstream says no"}`)
			_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
			if tc.status < 300 {
				if err != nil {
					t.Fatalf("2xx should not return an error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("non-2xx should return an error")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("incorrect permanent classification for HTTP %d: want %v, got %v (%v)",
					tc.status, tc.permanent, got, err)
			}
			// Include the status code so users can distinguish a local config issue
			// from a remote outage. Assert the language-independent number, not StatusText.
			if !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Errorf("error should include HTTP status %d; got %v", tc.status, err)
			}
		})
	}
}

// TestDoJSONIncludesResponseSnippet verifies that the remote error detail is
// included so users can tell why the request was rejected.
func TestDoJSONIncludesResponseSnippet(t *testing.T) {
	srv := replyServer(t, 400, `{"error":"invalid webhook token"}`)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "invalid webhook token") {
		t.Errorf("error should include the remote detail; got %v", err)
	}
}

// TestDoJSONSnippetIsSingleLineAndBounded constrains the snippet stored in
// last_error and shown in the UI: it must be single-line and bounded.
func TestDoJSONSnippetIsSingleLineAndBounded(t *testing.T) {
	// A response containing newlines, tabs, and 5000 characters.
	long := strings.Repeat("x", 5000)
	srv := replyServer(t, 500, "line1\nline2\r\n\tline3 "+long)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\r\n\t") {
		t.Errorf("error should be collapsed to one line; got %q", msg)
	}
	// The snippet is limited to 200 characters plus a fixed prefix.
	if len(msg) > 400 {
		t.Errorf("error is too long (%d bytes); snippet should be truncated: %q", len(msg), msg)
	}
}

// TestDoJSONRejectsOversizedResponse verifies that a huge remote response is
// bounded instead of being fully loaded and stored in delivery history.
func TestDoJSONRejectsOversizedResponse(t *testing.T) {
	huge := strings.Repeat("A", 1<<20) // 1 MiB
	srv := replyServer(t, 400, huge)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(err.Error()) > 400 {
		t.Errorf("oversized response should be read with a limit and truncated; error length is %d", len(err.Error()))
	}
}
