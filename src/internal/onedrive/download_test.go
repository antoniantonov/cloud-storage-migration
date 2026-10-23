package onedrive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antoniantonov/cloud-storage-migration/src/internal/model"
)

func stagingFile(t *testing.T) *os.File {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(workDir(t), "staging"), os.O_CREATE|os.O_RDWR|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func sourceFile(size *int64) model.SourceFile {
	return model.SourceFile{
		ID: "item", Name: "file.bin", MIMEType: "application/octet-stream", Size: size,
		DownloadURL: "https://download.example/content?secret=never-log",
	}
}

func intSize(size int64) *int64 { return &size }

func downloadResponse(status int, body io.ReadCloser, length int64) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: body, ContentLength: length}
}

func downloader(transport roundTripFunc) *Client {
	return &Client{download: &http.Client{Transport: transport}}
}

func TestDownloadStreamsAndLeavesStagingFileOpen(t *testing.T) {
	payload := []byte("file bytes\x00\x01\xff")
	file := stagingFile(t)
	if _, err := file.WriteString("previous partial download longer than payload"); err != nil {
		t.Fatal(err)
	}
	client := downloader(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
			t.Fatal("download request leaked credentials or changed method")
		}
		if request.Header.Get("Accept-Encoding") != "identity" {
			t.Fatal("download did not require unmodified bytes")
		}
		return downloadResponse(200, io.NopCloser(bytes.NewReader(payload)), int64(len(payload))), nil
	})
	if err := client.Download(context.Background(), sourceFile(intSize(int64(len(payload)))), file); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("provider closed caller-owned staging file: %v", err)
	}
	data, err := io.ReadAll(file)
	if err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("download bytes %q, %v", data, err)
	}
}

func TestDownloadStatusesAndRetryBounds(t *testing.T) {
	for _, test := range []struct {
		status   int
		attempts int
	}{
		{http.StatusPartialContent, 1}, {http.StatusNotFound, 1}, {http.StatusBadRequest, 1},
		{http.StatusRequestTimeout, downloadAttempts}, {http.StatusTooManyRequests, downloadAttempts},
		{http.StatusBadGateway, downloadAttempts}, {http.StatusServiceUnavailable, downloadAttempts},
	} {
		t.Run(fmt.Sprint(test.status), func(t *testing.T) {
			calls := 0
			client := downloader(func(request *http.Request) (*http.Response, error) {
				calls++
				return downloadResponse(test.status, io.NopCloser(strings.NewReader("SECRET BODY")), -1), nil
			})
			err := client.Download(context.Background(), sourceFile(nil), stagingFile(t))
			if err == nil || calls != test.attempts || !strings.Contains(err.Error(), fmt.Sprint(test.status)) || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "never-log") {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
		})
	}
}

func TestDownloadRefreshesExpiredURLThroughSDK(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			metadataCalls, downloadCalls := 0, 0
			client := testClient(t, func(request *http.Request) (*http.Response, error) {
				metadataCalls++
				if request.URL.Path != "/v1.0/drives/drive/items/item" {
					t.Fatalf("refresh did not use opaque ID lookup: %s", request.URL.Path)
				}
				return jsonResponse(request, 200, `{"id":"item","name":"file.bin","size":5,"file":{},"@microsoft.graph.downloadUrl":"https://download.example/new?secret=refreshed"}`), nil
			})
			client.download = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				downloadCalls++
				if request.Header.Get("Authorization") != "" {
					t.Fatal("Graph authorization leaked to content host")
				}
				if downloadCalls == 1 {
					return downloadResponse(status, io.NopCloser(strings.NewReader("expired")), 7), nil
				}
				if request.URL.Path != "/new" {
					t.Fatal("did not use refreshed URL")
				}
				return downloadResponse(200, io.NopCloser(strings.NewReader("bytes")), 5), nil
			})}
			if err := client.Download(context.Background(), sourceFile(intSize(5)), stagingFile(t)); err != nil || metadataCalls != 1 || downloadCalls != 2 {
				t.Fatalf("refresh failed: metadata=%d downloads=%d error=%v", metadataCalls, downloadCalls, err)
			}
		})
	}
}

func TestDownloadRefreshRejectsChangedOrIneligibleFile(t *testing.T) {
	for _, fragment := range []string{
		`"name":"changed.bin","file":{},"size":5`,
		`"name":"file.bin","file":{},"size":6`,
		`"name":"file.bin","folder":{},"size":5`,
		`"name":"file.bin","remoteItem":{},"file":{},"size":5`,
		`"name":"file.bin","file":{"mimeType":"changed/type"},"size":5`,
	} {
		t.Run(fragment, func(t *testing.T) {
			client := testClient(t, func(request *http.Request) (*http.Response, error) {
				return jsonResponse(request, 200, `{"id":"item","@microsoft.graph.downloadUrl":"https://download.example/new",`+fragment+`}`), nil
			})
			calls := 0
			client.download = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				return downloadResponse(403, io.NopCloser(strings.NewReader("")), 0), nil
			})}
			if err := client.Download(context.Background(), sourceFile(intSize(5)), stagingFile(t)); err == nil || calls != 1 {
				t.Fatalf("accepted changed source: calls=%d error=%v", calls, err)
			}
		})
	}
}

type failingBody struct {
	body io.Reader
	err  error
}

func (b *failingBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if err == io.EOF {
		return n, b.err
	}
	return n, err
}
func (b *failingBody) Close() error { return nil }

func TestDownloadRestartsAfterPartialTransportFailure(t *testing.T) {
	calls := 0
	client := downloader(func(request *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return downloadResponse(200, &failingBody{body: strings.NewReader("partial"), err: io.ErrUnexpectedEOF}, 10), nil
		}
		return downloadResponse(200, io.NopCloser(strings.NewReader("0123456789")), 10), nil
	})
	file := stagingFile(t)
	if err := client.Download(context.Background(), sourceFile(intSize(10)), file); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil || string(data) != "0123456789" || calls != 2 {
		t.Fatalf("partial file was reused: %q calls=%d error=%v", data, calls, err)
	}
}

func TestDownloadDetectsLengthAndEncodingFailures(t *testing.T) {
	for _, test := range []struct {
		name     string
		size     *int64
		length   int64
		body     string
		encoding string
	}{
		{"Content-Length mismatch", intSize(10), 5, "12345", ""},
		{"truncated body", intSize(10), 10, "12345", ""},
		{"too much data", intSize(5), -1, "123456", ""},
		{"short unknown length", intSize(10), -1, "12345", ""},
		{"unknown size but truncated HTTP", nil, 10, "12345", ""},
		{"encoded bytes", intSize(5), 5, "12345", "gzip"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := downloader(func(request *http.Request) (*http.Response, error) {
				response := downloadResponse(200, io.NopCloser(strings.NewReader(test.body)), test.length)
				response.Header.Set("Content-Encoding", test.encoding)
				return response, nil
			})
			if err := client.Download(context.Background(), sourceFile(test.size), stagingFile(t)); err == nil {
				t.Fatal("accepted an incomplete or modified download")
			}
		})
	}
}

func TestDownloadUnknownSizeAndEmptyFiles(t *testing.T) {
	for _, size := range []*int64{nil, intSize(0)} {
		client := downloader(func(request *http.Request) (*http.Response, error) {
			return downloadResponse(200, io.NopCloser(strings.NewReader("")), -1), nil
		})
		if err := client.Download(context.Background(), sourceFile(size), stagingFile(t)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDownloadCancellationAndErrorRedaction(t *testing.T) {
	t.Run("transport URL redacted", func(t *testing.T) {
		client := downloader(func(request *http.Request) (*http.Response, error) {
			return nil, errors.New("SECRET transport message " + request.URL.String())
		})
		err := client.Download(context.Background(), sourceFile(nil), stagingFile(t))
		if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "download.example") {
			t.Fatalf("leaked content URL: %v", err)
		}
	})
	t.Run("cancel transport", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		client := downloader(func(request *http.Request) (*http.Response, error) {
			cancel()
			return nil, request.Context().Err()
		})
		if err := client.Download(ctx, sourceFile(nil), stagingFile(t)); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error: %v", err)
		}
	})
	t.Run("cancel backoff", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := waitRetry(ctx, time.Hour); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
	t.Run("closed staging file", func(t *testing.T) {
		file := stagingFile(t)
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		client := downloader(func(*http.Request) (*http.Response, error) {
			t.Fatal("network called after staging file error")
			return nil, nil
		})
		if err := client.Download(context.Background(), sourceFile(nil), file); err == nil {
			t.Fatal("accepted closed staging file")
		}
	})
}

type closeErrorBody struct{ io.Reader }

func (b closeErrorBody) Close() error { return errors.New("SECRET close failure") }

func TestDownloadResponseCloseFailure(t *testing.T) {
	client := downloader(func(request *http.Request) (*http.Response, error) {
		return downloadResponse(200, closeErrorBody{strings.NewReader("ok")}, 2), nil
	})
	if err := client.Download(context.Background(), sourceFile(intSize(2)), stagingFile(t)); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("close failure: %v", err)
	}
}

type generatedReader struct {
	remaining int64
	maxRead   int
}

func (r *generatedReader) Read(p []byte) (int, error) {
	if len(p) > r.maxRead {
		r.maxRead = len(p)
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > r.remaining {
		n = int(r.remaining)
	}
	for i := range p[:n] {
		p[i] = 'x'
	}
	r.remaining -= int64(n)
	return n, nil
}
func (r *generatedReader) Close() error { return nil }

func TestDownloadLargeGeneratedFileUsesBoundedBuffer(t *testing.T) {
	const size = 64 * 1024 * 1024
	reader := &generatedReader{remaining: size}
	client := downloader(func(*http.Request) (*http.Response, error) {
		return downloadResponse(200, reader, size), nil
	})
	file := stagingFile(t)
	if err := client.Download(context.Background(), sourceFile(intSize(size)), file); err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil || info.Size() != size || reader.maxRead > 128*1024 {
		t.Fatalf("unbounded/incomplete transfer: info=%v max read=%d error=%v", info, reader.maxRead, err)
	}
}

func TestDownloadTLSRedirectDoesNotSendAuthorization(t *testing.T) {
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
			t.Error("download transmitted authorization or cookies")
		}
		if request.URL.Path == "/start" {
			http.Redirect(writer, request, "/content", http.StatusFound)
			return
		}
		writer.Header().Set("Content-Length", "5")
		_, _ = io.WriteString(writer, "bytes")
	}))
	defer server.Close()
	client := downloadClient()
	client.Transport = server.Client().Transport
	provider := &Client{download: client}
	file := sourceFile(intSize(5))
	file.DownloadURL = server.URL + "/start"
	if err := provider.Download(context.Background(), file, stagingFile(t)); err != nil || requests != 2 {
		t.Fatalf("secure redirected download: requests=%d error=%v", requests, err)
	}
}

func TestDownloadURLAndRedirectValidation(t *testing.T) {
	for _, raw := range []string{
		"", "http://download.example/secret", "//download.example/secret", "https://user:password@download.example",
		"https://download.example/#secret", "https:///missing-host", "file:///secret", "https://[bad",
	} {
		if err := validateDownloadURL(raw); err == nil || strings.Contains(err.Error(), raw) && raw != "" {
			t.Fatalf("invalid URL accepted or leaked: %q, %v", raw, err)
		}
	}
	client := downloadClient()
	request, _ := http.NewRequest(http.MethodGet, "http://insecure.example/secret", nil)
	if err := client.CheckRedirect(request, nil); err == nil {
		t.Fatal("accepted insecure redirect")
	}
	request, _ = http.NewRequest(http.MethodGet, "https://download.example/content", nil)
	request.Header.Set("Authorization", "Bearer SECRET")
	request.Header.Set("Cookie", "SECRET")
	if err := client.CheckRedirect(request, nil); err != nil || request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
		t.Fatal("did not strip redirect credentials")
	}
	if err := client.CheckRedirect(request, make([]*http.Request, 10)); err == nil {
		t.Fatal("accepted unbounded redirect chain")
	}
}

func TestDownloadRetryAfter(t *testing.T) {
	for value, want := range map[string]time.Duration{
		"": 0, "garbage": 0, "-5": 0, "0": 0, "2": 2 * time.Second,
		"+2": 2 * time.Second, "00002": 2 * time.Second, " 2 ": 2 * time.Second, "30": 30 * time.Second,
	} {
		if got, err := retryAfter(value); err != nil || got != want {
			t.Fatalf("Retry-After %q: %v, %v; want %v", value, got, err, want)
		}
	}
	for _, value := range []string{
		"31", " 31 ", "99999", "2147483648", "9223372036854775808", "18446744073709551616",
		strings.Repeat("9", 200), "+" + strings.Repeat("9", 200),
		time.Now().Add(time.Hour).UTC().Format(http.TimeFormat),
		"Fri, 31 Dec 9999 23:59:59 GMT",
	} {
		if got, err := retryAfter(value); got != 0 || err == nil || !strings.Contains(err.Error(), "retry later") {
			t.Fatalf("long Retry-After %q: %v, %v; want explicit retry-later error", value, got, err)
		}
	}
	if got, err := retryAfter(time.Now().Add(20 * time.Second).UTC().Format(http.TimeFormat)); err != nil || got <= 0 || got > 20*time.Second {
		t.Fatalf("short HTTP-date not honored: %v, %v", got, err)
	}
	if got, err := retryAfter(time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)); err != nil || got != 0 {
		t.Fatalf("past HTTP-date caused a delay: %v, %v", got, err)
	}
}

func TestDownloadDoesNotRetryEarlyForLongRetryAfter(t *testing.T) {
	for _, value := range []string{
		"31", "99999", "2147483648", "18446744073709551616", strings.Repeat("9", 200),
		time.Now().Add(time.Hour).UTC().Format(http.TimeFormat),
	} {
		for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusForbidden} {
			t.Run(fmt.Sprintf("%d/%s", status, value), func(t *testing.T) {
				calls := 0
				client := downloader(func(request *http.Request) (*http.Response, error) {
					calls++
					if calls > 1 {
						return downloadResponse(200, io.NopCloser(strings.NewReader("success")), 7), nil
					}
					response := downloadResponse(status, io.NopCloser(strings.NewReader("SECRET BODY")), -1)
					response.Header.Set("Retry-After", value)
					return response, nil
				})
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				err := client.Download(ctx, sourceFile(nil), stagingFile(t))
				if calls != 1 || err == nil || !strings.Contains(err.Error(), "retry later") || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("long server delay was ignored or waited out: calls=%d error=%v", calls, err)
				}
				if !strings.Contains(err.Error(), fmt.Sprint(status)) || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "never-log") {
					t.Fatalf("lost status or leaked response details: %v", err)
				}
			})
		}
	}
}
