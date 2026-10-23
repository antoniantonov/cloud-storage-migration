package onedrive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/antoniantonov/cloud-storage-migration/src/internal/model"
)

const downloadAttempts = 3

func downloadClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many download redirects")
			}
			if err := validateDownloadURL(req.URL.String()); err != nil {
				return err
			}
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			return nil
		},
	}
}

func validateDownloadURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return errors.New("OneDrive returned a missing or invalid secure download URL")
	}
	return nil
}

func (c *Client) Download(ctx context.Context, source model.SourceFile, file *os.File) error {
	if file == nil {
		return errors.New("OneDrive download requires an open staging file")
	}
	if source.Size != nil && *source.Size < 0 {
		return errors.New("OneDrive download has an invalid negative source size")
	}
	current := source
	for attempt := 0; attempt < downloadAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := validateDownloadURL(current.DownloadURL); err != nil {
			return err
		}
		if err := file.Truncate(0); err != nil {
			return fmt.Errorf("truncate OneDrive staging file: %w", err)
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("rewind OneDrive staging file: %w", err)
		}
		retry, refresh, delay, err := c.downloadOnce(ctx, current, file)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !retry || attempt+1 == downloadAttempts {
			return err
		}
		if delay == 0 {
			delay = c.retryDelay * time.Duration(1<<attempt)
		}
		if err := waitRetry(ctx, delay); err != nil {
			return err
		}
		if refresh {
			replacement, err := c.File(ctx, source.ID)
			if err != nil {
				return err
			}
			if replacement.Name != source.Name || replacement.MIMEType != source.MIMEType || !sameSize(replacement.Size, source.Size) {
				return errors.New("OneDrive file changed while refreshing its download URL; retry the selection")
			}
			current = replacement
		}
	}
	return errors.New("OneDrive download retry limit reached")
}

func (c *Client) downloadOnce(ctx context.Context, source model.SourceFile, file *os.File) (retry, refresh bool, delay time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.DownloadURL, nil)
	if err != nil {
		return false, false, 0, safeFailure("prepare OneDrive download", err)
	}
	req.Header.Set("Accept-Encoding", "identity")
	response, err := c.download.Do(req)
	if err != nil {
		return true, false, 0, safeFailure("stream OneDrive download", err)
	}
	if response.StatusCode != http.StatusOK {
		closeErr := response.Body.Close()
		status := response.StatusCode
		retry = status == 401 || status == 403 || status == 408 || status == 429 || status >= 500
		refresh = status == 401 || status == 403
		err = fmt.Errorf("OneDrive download returned HTTP %d", status)
		if closeErr != nil {
			err = errors.Join(err, safeFailure("close OneDrive download response", closeErr))
		}
		if retry {
			var retryErr error
			delay, retryErr = retryAfter(response.Header.Get("Retry-After"))
			if retryErr != nil {
				return false, false, 0, errors.Join(err, retryErr)
			}
		}
		return retry, refresh, delay, err
	}
	if response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity" {
		_ = response.Body.Close()
		return false, false, 0, errors.New("OneDrive download returned an unexpected content encoding")
	}
	if source.Size != nil && response.ContentLength >= 0 && response.ContentLength != *source.Size {
		_ = response.Body.Close()
		return true, false, 0, errors.New("OneDrive download Content-Length does not match source size")
	}
	reader := io.Reader(response.Body)
	if source.Size != nil && *source.Size < math.MaxInt64 {
		reader = io.LimitReader(reader, *source.Size+1)
	}
	// Hide os.File.ReadFrom so io.CopyBuffer always uses a bounded transfer buffer.
	written, copyErr := io.CopyBuffer(struct{ io.Writer }{file}, reader, make([]byte, 128*1024))
	closeErr := response.Body.Close()
	if copyErr != nil {
		var diskErr *os.PathError
		if errors.As(copyErr, &diskErr) {
			return false, false, 0, fmt.Errorf("write OneDrive staging file: %w", diskErr)
		}
		return true, false, 0, safeFailure("read OneDrive download body", copyErr)
	}
	if closeErr != nil {
		return true, false, 0, safeFailure("close OneDrive download response", closeErr)
	}
	if (response.ContentLength >= 0 && written != response.ContentLength) || (source.Size != nil && written != *source.Size) {
		return true, false, 0, errors.New("OneDrive download length does not match the complete source file")
	}
	return false, false, 0, nil
}

func sameSize(a, b *int64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func retryAfter(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	digits := strings.TrimPrefix(value, "+")
	var delay time.Duration
	if digits != "" && strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
		seconds, err := strconv.ParseUint(digits, 10, 64)
		if err != nil || seconds > 30 {
			return 0, errors.New("OneDrive server requested a Retry-After delay longer than 30 seconds; retry later")
		}
		delay = time.Duration(seconds) * time.Second
	} else if date, err := http.ParseTime(value); err == nil {
		delay = time.Until(date)
	}
	if delay > 30*time.Second {
		return 0, errors.New("OneDrive server requested a Retry-After delay longer than 30 seconds; retry later")
	}
	if delay < 0 {
		return 0, nil
	}
	return delay, nil
}

func waitRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
