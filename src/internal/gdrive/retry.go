package gdrive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/api/googleapi"
)

const maxRetryDelay = 30 * time.Second

type retryPolicy struct {
	attempts int
	sleep    func(context.Context, time.Duration) error
}

func defaultRetryPolicy() retryPolicy { return retryPolicy{attempts: 4, sleep: sleepContext} }

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func retryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var safe *providerError
	if errors.As(err, &safe) {
		return false
	}
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		if apiErr.Code == http.StatusForbidden {
			if len(apiErr.Errors) == 0 {
				return false
			}
			for _, detail := range apiErr.Errors {
				if detail.Reason != "rateLimitExceeded" && detail.Reason != "userRateLimitExceeded" {
					return false
				}
			}
			return true
		}
		return apiErr.Code == http.StatusTooManyRequests || apiErr.Code == http.StatusRequestTimeout ||
			(apiErr.Code >= 500 && apiErr.Code <= 599)
	}
	var networkErr net.Error
	return (errors.As(err, &networkErr) && networkErr.Timeout()) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE)
}

func retryAfter(value string, now time.Time) time.Duration {
	trimmed := strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds > int64(maxRetryDelay/time.Second) {
			return maxRetryDelay + time.Second
		}
		return time.Duration(seconds) * time.Second
	} else if errors.Is(err, strconv.ErrRange) && strings.Trim(trimmed, "0123456789") == "" {
		// Overflowing positive delta-seconds must not permit an earlier retry.
		return maxRetryDelay + time.Second
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return date.Sub(now)
	}
	return 0
}

func retryCall[T any](ctx context.Context, policy retryPolicy, call func() (T, error)) (T, error) {
	var zero T
	for attempt := 0; attempt < policy.attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		value, err := call()
		if err == nil {
			return value, nil
		}
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		if !retryable(err) || attempt+1 == policy.attempts {
			return zero, err
		}
		delay := time.Duration(1<<attempt)*500*time.Millisecond + time.Duration(rand.Int64N(int64(250*time.Millisecond)))
		var apiErr *googleapi.Error
		if errors.As(err, &apiErr) {
			if required := retryAfter(apiErr.Header.Get("Retry-After"), time.Now()); required > delay {
				delay = required
			}
		}
		if delay > maxRetryDelay {
			return zero, diagnostic("Google requested a longer retry delay; retry the command later")
		}
		if err := policy.sleep(ctx, delay); err != nil {
			return zero, err
		}
	}
	return zero, diagnostic("Google retry policy has no attempts")
}

func driveError(operation string, err error) error {
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("Google Drive %s: %w", operation, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("Google Drive %s: %w", operation, context.DeadlineExceeded)
	}
	var safe *providerError
	if errors.As(err, &safe) {
		return fmt.Errorf("Google Drive %s: %w", operation, safe)
	}
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		if apiErr.Code == http.StatusUnauthorized {
			return diagnostic("Google Drive " + operation + " rejected authorization (HTTP 401); remove this Desktop client's Google token cache (google-*.json) from AUTH_DIR, then rerun with AUTH_INTERACTIVE=true to sign in")
		}
		return diagnostic(fmt.Sprintf("Google Drive %s failed (HTTP %d; provider details suppressed)", operation, apiErr.Code))
	}
	return diagnostic("Google Drive " + operation + " failed (network, local I/O, or invalid response; sensitive provider details suppressed)")
}

// The SDK owns chunk retransmission and its deadline. It does not observe
// Retry-After, so wait before handing throttled chunk responses back to it.
type chunkRetryAfterTransport struct {
	base  http.RoundTripper
	sleep func(context.Context, time.Duration) error
}

func (t chunkRetryAfterTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil || response == nil || request.Header.Get("Content-Range") == "" ||
		(response.StatusCode != 429 && response.StatusCode != 408 && (response.StatusCode < 500 || response.StatusCode > 599)) {
		return response, err
	}
	delay := retryAfter(response.Header.Get("Retry-After"), time.Now())
	if delay > maxRetryDelay {
		response.Body.Close()
		return nil, diagnostic("Google requested a longer chunk retry delay; retry the command later")
	}
	if delay > 0 {
		if err := t.sleep(request.Context(), delay); err != nil {
			response.Body.Close()
			return nil, err
		}
	}
	return response, nil
}
