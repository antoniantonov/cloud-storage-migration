package gdrive

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
)

func TestRetryClassification(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{"429", &googleapi.Error{Code: 429}, true},
		{"503", &googleapi.Error{Code: 503}, true},
		{"408", &googleapi.Error{Code: 408}, true},
		{"rate limit", &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "rateLimitExceeded"}}}, true},
		{"user rate limit", &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "userRateLimitExceeded"}}}, true},
		{"storage quota", &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "storageQuotaExceeded"}}}, false},
		{"permanent 403", &googleapi.Error{Code: 403}, false},
		{"mixed permission and throttle", &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "rateLimitExceeded"}, {Reason: "insufficientFilePermissions"}}}, false},
		{"404", &googleapi.Error{Code: 404}, false},
		{"409", &googleapi.Error{Code: 409}, false},
		{"401", &googleapi.Error{Code: 401}, false},
		{"cancelled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"EOF", io.ErrUnexpectedEOF, true},
		{"reset", &url.Error{Op: "Get", URL: "https://private.invalid", Err: syscall.ECONNRESET}, true},
		{"untrusted error", errors.New("PRIVATE_RESPONSE"), false},
		{"authentication error", reauthorization(), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := retryable(test.err); got != test.want {
				t.Errorf("retryable=%v want %v", got, test.want)
			}
		})
	}
}

func TestBoundedReadRetriesRespectRetryAfterAndCancellation(t *testing.T) {
	attempts := 0
	var delays []time.Duration
	policy := retryPolicy{attempts: 4, sleep: func(ctx context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return ctx.Err()
	}}
	value, err := retryCall(context.Background(), policy, func() (int, error) {
		attempts++
		if attempts < 3 {
			return 0, &googleapi.Error{Code: 429, Header: http.Header{"Retry-After": {"4"}}}
		}
		return 42, nil
	})
	if err != nil || value != 42 || attempts != 3 || len(delays) != 2 || delays[0] < 4*time.Second || delays[1] < 4*time.Second {
		t.Fatalf("value=%d attempts=%d delays=%v err=%v", value, attempts, delays, err)
	}
	attempts, delays = 0, nil
	_, err = retryCall(context.Background(), policy, func() (int, error) {
		attempts++
		return 0, &googleapi.Error{Code: 503}
	})
	if err == nil || attempts != 4 || len(delays) != 3 {
		t.Fatalf("unbounded retries: attempts=%d delays=%v err=%v", attempts, delays, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	attempts = 0
	_, err = retryCall(ctx, retryPolicy{attempts: 4, sleep: sleepContext}, func() (int, error) {
		attempts++
		cancel()
		return 0, &googleapi.Error{Code: 503}
	})
	if !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatalf("cancellation ignored: %d %v", attempts, err)
	}
	for _, header := range []string{
		"31", "3600", "9223372036854775808", strings.Repeat("9", 1024),
		time.Now().Add(time.Hour).UTC().Format(http.TimeFormat),
	} {
		attempts, delays = 0, nil
		_, err = retryCall(context.Background(), policy, func() (int, error) {
			attempts++
			return 0, &googleapi.Error{Code: 429, Header: http.Header{"Retry-After": {header}}}
		})
		if err == nil || attempts != 1 || len(delays) != 0 || !strings.Contains(err.Error(), "later") {
			t.Fatalf("long throttle must fail without sleeping or retrying: attempts=%d delays=%v err=%v", attempts, delays, err)
		}
	}
}

func TestRetryAfterParsing(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"seconds", "7", 7 * time.Second},
		{"maximum supported", "30", maxRetryDelay},
		{"above supported", "31", maxRetryDelay + time.Second},
		{"negative seconds", "-1", 0},
		{"zero", "0", 0},
		{"invalid", "invalid", 0},
		{"future date", now.Add(10 * time.Second).Format(http.TimeFormat), 10 * time.Second},
		{"long future date", now.Add(time.Hour).Format(http.TimeFormat), time.Hour},
		{"past date", now.Add(-time.Hour).Format(http.TimeFormat), 0},
		{"maximum int64", "9223372036854775807", maxRetryDelay + time.Second},
		{"int64 overflow", "9223372036854775808", maxRetryDelay + time.Second},
		{"arbitrarily large integer", strings.Repeat("9", 1024), maxRetryDelay + time.Second},
		{"trimmed overflow", "\t9223372036854775808 ", maxRetryDelay + time.Second},
		{"many leading zeroes", strings.Repeat("0", 1024) + "7", 7 * time.Second},
		{"all zeroes", strings.Repeat("0", 1024), 0},
		{"negative overflow", "-9223372036854775809", 0},
		{"non-digit overflow suffix", strings.Repeat("9", 1024) + "x", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := retryAfter(test.value, now); got != test.want {
				t.Errorf("Retry-After = %v, want %v", got, test.want)
			}
		})
	}
}

func TestChunkRetryAfterHonorsWaitAndBound(t *testing.T) {
	for _, header := range []string{
		"5", "3600", "9223372036854775808", strings.Repeat("9", 1024),
		time.Now().Add(time.Hour).UTC().Format(http.TimeFormat),
	} {
		var delay time.Duration
		transport := chunkRetryAfterTransport{
			base: roundTripFunc(func(*http.Request) (*http.Response, error) {
				res := response(429, `{"error":{"code":429}}`)
				res.Header.Set("Retry-After", header)
				return res, nil
			}),
			sleep: func(ctx context.Context, d time.Duration) error { delay = d; return ctx.Err() },
		}
		request, _ := http.NewRequest("PUT", "https://upload.invalid/session", nil)
		request.Header.Set("Content-Range", "bytes 0-5/*")
		result, err := transport.RoundTrip(request)
		if header == "5" {
			if err != nil || result == nil || delay != 5*time.Second {
				t.Fatalf("chunk ignored throttle: delay=%v err=%v", delay, err)
			}
			result.Body.Close()
		} else if result != nil || err == nil || retryable(err) || delay != 0 || !strings.Contains(err.Error(), "later") {
			t.Fatalf("long throttle was retryable: %v %v", result, err)
		}
	}
}

func TestDriveErrorsDoNotLeakSDKOrSessionURLs(t *testing.T) {
	for _, raw := range []error{
		&googleapi.Error{Code: 403, Body: "ACCESS_TOKEN_PRIVATE", Message: "https://private.invalid", Errors: []googleapi.ErrorItem{{Reason: "PRIVATE_RESPONSE"}}},
		&url.Error{Op: "Put", URL: "https://private.invalid/session", Err: errors.New("ACCESS_TOKEN_PRIVATE")},
		errors.New("upload request to https://private.invalid not sent"),
	} {
		err := driveError("upload", raw)
		assertNoSecrets(t, err.Error())
	}
	if err := driveError("upload", &googleapi.Error{Code: 401}); !strings.Contains(err.Error(), "AUTH_INTERACTIVE=true") {
		t.Fatalf("401 lacks actionable authorization advice: %v", err)
	}
	if err := driveError("upload", context.Canceled); !errors.Is(err, context.Canceled) {
		t.Errorf("lost context cancellation: %v", err)
	}
}
