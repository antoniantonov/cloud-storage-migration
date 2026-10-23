package gdrive

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/antoniantonov/cloud-storage-migration/src/internal/migration"
	"github.com/antoniantonov/cloud-storage-migration/src/internal/model"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

var _ migration.Destination = (*Client)(nil)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func jsonResponse(value any) *http.Response {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return response(200, string(data))
}

func noSleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

func sdkClient(t *testing.T, transport roundTripFunc) *Client {
	t.Helper()
	service, err := drive.NewService(context.Background(),
		option.WithEndpoint("https://drive.invalid/drive/v3/"),
		option.WithHTTPClient(&http.Client{Transport: chunkRetryAfterTransport{base: transport, sleep: noSleep}}),
		option.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	return &Client{service: service, retry: retryPolicy{attempts: 4, sleep: noSleep}}
}

func privateTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", ".gdrive-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}

func localFile(t *testing.T, data []byte) (*os.File, string) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(privateTestDir(t), "data"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
	hash := md5.Sum(data)
	return file, hex.EncodeToString(hash[:])
}

func readMultipart(t *testing.T, request *http.Request) (drive.File, []byte) {
	t.Helper()
	var metadata drive.File
	contentType, params, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || contentType != "multipart/related" {
		t.Errorf("multipart content type: %q, %v", contentType, err)
		return metadata, nil
	}
	reader := multipart.NewReader(request.Body, params["boundary"])
	part, err := reader.NextPart()
	if err != nil {
		t.Error(err)
		return metadata, nil
	}
	if err := json.NewDecoder(part).Decode(&metadata); err != nil {
		t.Error(err)
	}
	part, err = reader.NextPart()
	if err != nil {
		t.Error(err)
		return metadata, nil
	}
	data, err := io.ReadAll(part)
	if err != nil {
		t.Error(err)
	}
	if _, err := reader.NextPart(); !errors.Is(err, io.EOF) {
		t.Errorf("unexpected extra media part: %v", err)
	}
	return metadata, data
}

func TestEnsureFolderPagesAndAmbiguity(t *testing.T) {
	for _, test := range []struct {
		name      string
		second    string
		wantID    string
		wantError string
	}{
		{"manual folder reused", `{"files":[{"id":"manual","name":"test-migration","mimeType":"application/vnd.google-apps.folder"}]}`, "manual", ""},
		{"same ID deduplicated", `{"files":[{"id":"manual","name":"test-migration","mimeType":"application/vnd.google-apps.folder"},{"id":"manual","name":"test-migration","mimeType":"application/vnd.google-apps.folder"}]}`, "manual", ""},
		{"all conflicting IDs reported", `{"files":[{"id":"z","name":"test-migration","mimeType":"application/vnd.google-apps.folder"},{"id":"a","name":"test-migration","mimeType":"application/vnd.google-apps.folder"}]}`, "", `["a" "z"]`},
		{"incomplete final search", `{"files":[],"incompleteSearch":true}`, "", "incomplete"},
		{"repeated next page", `{"files":[],"nextPageToken":"page-two"}`, "", "repeated"},
		{"invalid file metadata", `{"files":[null]}`, "", "invalid file metadata"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client := sdkClient(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" || r.URL.Path != "/drive/v3/files" {
					t.Errorf("unexpected cloud mutation: %s %s", r.Method, r.URL.Path)
				}
				query := r.URL.Query()
				if query.Get("q") != folderQuery || query.Get("spaces") != "drive" ||
					query.Get("corpora") != "user" || query.Get("includeItemsFromAllDrives") != "false" {
					t.Errorf("incorrect root discovery query: %v", query)
				}
				if !strings.Contains(query.Get("fields"), "incompleteSearch") {
					t.Error("incompleteSearch must be requested")
				}
				if calls == 1 {
					if query.Get("pageToken") != "" {
						t.Error("unexpected initial page token")
					}
					return response(200, `{"files":[],"nextPageToken":"page-two"}`), nil
				}
				if query.Get("pageToken") != "page-two" {
					t.Errorf("page token: %q", query.Get("pageToken"))
				}
				return response(200, test.second), nil
			})
			id, err := client.EnsureFolder(context.Background())
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("expected %q, got %v", test.wantError, err)
				}
			} else if err != nil || id != test.wantID {
				t.Fatalf("got %q, %v", id, err)
			}
			if calls != 2 {
				t.Errorf("expected complete pagination, got %d requests", calls)
			}
		})
	}
}

func TestEnsureFolderCreatesRootWithoutTouchingNonFolders(t *testing.T) {
	requests := 0
	client := sdkClient(t, func(r *http.Request) (*http.Response, error) {
		requests++
		switch {
		case r.Method == "GET" && r.URL.Path == "/drive/v3/files":
			return response(200, `{"files":[
				{"id":"regular","name":"test-migration","mimeType":"text/plain"},
				{"id":"trash","name":"test-migration","mimeType":"application/vnd.google-apps.folder","trashed":true},
				{"id":"other","name":"Test-Migration","mimeType":"application/vnd.google-apps.folder"}
			]}`), nil
		case r.Method == "GET" && r.URL.Path == "/drive/v3/files/generateIds":
			if r.URL.Query().Get("count") != "1" || r.URL.Query().Get("space") != "drive" || r.URL.Query().Get("type") != "files" {
				t.Error("incorrect ID reservation")
			}
			return response(200, `{"ids":["reserved"]}`), nil
		case r.Method == "POST" && r.URL.Path == "/drive/v3/files":
			var file drive.File
			if err := json.NewDecoder(r.Body).Decode(&file); err != nil {
				t.Error(err)
			}
			if file.Id != "reserved" || file.Name != folderName || file.MimeType != folderMIME ||
				!reflect.DeepEqual(file.Parents, []string{"root"}) {
				t.Errorf("incorrect folder metadata: %+v", file)
			}
			return response(200, `{"id":"reserved"}`), nil
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			return response(500, `{}`), nil
		}
	})
	id, err := client.EnsureFolder(context.Background())
	if err != nil || id != "reserved" || requests != 3 {
		t.Fatalf("got id=%q requests=%d err=%v", id, requests, err)
	}
}

func TestFolderAmbiguousCreateVerifiesReservedID(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(fmt.Sprintf("mismatch=%v", mismatch), func(t *testing.T) {
			creates, reserved := 0, 0
			client := sdkClient(t, func(r *http.Request) (*http.Response, error) {
				switch {
				case r.Method == "GET" && r.URL.Path == "/drive/v3/files":
					return response(200, `{}`), nil
				case r.URL.Path == "/drive/v3/files/generateIds":
					reserved++
					return response(200, `{"ids":["same-id"]}`), nil
				case r.Method == "POST":
					creates++
					var file drive.File
					if err := json.NewDecoder(r.Body).Decode(&file); err != nil || file.Id != "same-id" {
						t.Errorf("create retry changed ID: %q %v", file.Id, err)
					}
					if creates == 1 {
						return response(503, `{"error":{"code":503,"message":"private"}}`), nil
					}
					return response(409, `{"error":{"code":409}}`), nil
				case r.URL.Path == "/drive/v3/files/same-id":
					parent := "actual-root"
					if mismatch {
						parent = "different-parent"
					}
					return jsonResponse(map[string]any{"id": "same-id", "name": folderName, "mimeType": folderMIME, "parents": []string{parent}}), nil
				case r.URL.Path == "/drive/v3/files/root":
					return response(200, `{"id":"actual-root"}`), nil
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					return response(500, `{}`), nil
				}
			})
			id, err := client.EnsureFolder(context.Background())
			if mismatch {
				if err == nil || !strings.Contains(err.Error(), "does not match") {
					t.Fatalf("mismatch not rejected: %v", err)
				}
			} else if err != nil || id != "same-id" {
				t.Fatalf("got %q %v", id, err)
			}
			if creates != 2 || reserved != 1 {
				t.Errorf("unsafe retries: creates=%d reserve=%d", creates, reserved)
			}
		})
	}
}

func TestExistingNamesAllTypesAndExactNames(t *testing.T) {
	count := 0
	client := sdkClient(t, func(r *http.Request) (*http.Response, error) {
		count++
		if r.Method != "GET" || r.URL.Query().Get("q") != `trashed = false and 'folder\\with\'quote' in parents` {
			t.Errorf("incorrect immediate child query: %s", r.URL.Query().Get("q"))
		}
		if count == 1 {
			return response(200, `{"files":[{"id":"1","name":"A"},{"id":"2","name":"a","mimeType":"application/vnd.google-apps.folder"}],"nextPageToken":"next"}`), nil
		}
		return response(200, `{"files":[{"id":"3","name":"shortcut","mimeType":"application/vnd.google-apps.shortcut"},{"id":"4","name":"A"},{"id":"5","name":"gone","trashed":true},{"id":"6","name":"a "}]}`), nil
	})
	names, err := client.ExistingNames(context.Background(), `folder\with'quote`)
	want := map[string]struct{}{"A": {}, "a": {}, "a ": {}, "shortcut": {}}
	if err != nil || !reflect.DeepEqual(names, want) || count != 2 {
		t.Fatalf("got %#v requests=%d err=%v", names, count, err)
	}
}

func TestDiscoveryFailureNeverCreates(t *testing.T) {
	for _, payload := range []string{
		`{"incompleteSearch":true}`,
		`{"error":{"code":403,"message":"PRIVATE_TOKEN https://secret.invalid/session"}}`,
	} {
		client := sdkClient(t, func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" || r.URL.Path != "/drive/v3/files" {
				t.Errorf("unexpected create after failed discovery: %s %s", r.Method, r.URL.Path)
			}
			status := 200
			if strings.Contains(payload, "403") {
				status = 403
			}
			return response(status, payload), nil
		})
		for _, operation := range []func() error{
			func() error { _, err := client.EnsureFolder(context.Background()); return err },
			func() error { _, err := client.ExistingNames(context.Background(), "folder"); return err },
		} {
			if err := operation(); err == nil || strings.Contains(err.Error(), "PRIVATE_TOKEN") || strings.Contains(err.Error(), "secret.invalid") {
				t.Fatalf("expected safe discovery failure, got %v", err)
			}
		}
	}
}

func TestBinaryMultipartUploadPreservesNameAndCallerFile(t *testing.T) {
	for _, mimeType := range []string{"text/plain", "", "application/vnd.google-apps.document", "invalid mime"} {
		t.Run(mimeType, func(t *testing.T) {
			data := []byte("preserve these original bytes\x00\n")
			file, checksum := localFile(t, data)
			name := "a name'with\\characters.txt"
			calls := 0
			client := sdkClient(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method == "GET" && r.URL.Path == "/drive/v3/files/generateIds" {
					return response(200, `{"ids":["new-file"]}`), nil
				}
				if r.Method != "POST" || r.URL.Path != "/upload/drive/v3/files" || r.URL.Query().Get("uploadType") != "multipart" {
					t.Errorf("unexpected upload request: %s %s", r.Method, r.URL)
				}
				if r.URL.Query().Get("fields") != uploadFields {
					t.Error("upload must request ID, size and checksum")
				}
				metadata, body := readMultipart(t, r)
				if metadata.Id != "new-file" || metadata.Name != name || metadata.MimeType != binaryMIME(mimeType) ||
					!reflect.DeepEqual(metadata.Parents, []string{"folder"}) || !bytes.Equal(body, data) {
					t.Errorf("unexpected upload metadata/data: %+v %q", metadata, body)
				}
				return jsonResponse(map[string]any{"id": "new-file", "size": fmt.Sprint(len(data)), "md5Checksum": checksum}), nil
			})
			result, err := client.Upload(context.Background(), "folder", model.SourceFile{Name: name, MIMEType: mimeType, DownloadURL: "https://secret.invalid/download"}, file, checksum)
			if err != nil || result.ID != "new-file" || result.MD5 != checksum || result.Size == nil || *result.Size != int64(len(data)) || calls != 2 {
				t.Fatalf("got %+v calls=%d err=%v", result, calls, err)
			}
			if _, err := file.Stat(); err != nil {
				t.Fatalf("upload closed caller's file: %v", err)
			}
		})
	}
}

func TestUploadAmbiguousCreateRetryIsIdempotent(t *testing.T) {
	for _, mismatch := range []string{"", "parent", "name", "mime", "size", "md5", "trashed", "shared", "id"} {
		t.Run(mismatch, func(t *testing.T) {
			data := []byte("same data on every attempt")
			file, checksum := localFile(t, data)
			creates, reserved := 0, 0
			client := sdkClient(t, func(r *http.Request) (*http.Response, error) {
				switch {
				case r.URL.Path == "/drive/v3/files/generateIds":
					reserved++
					return response(200, `{"ids":["reserved"]}`), nil
				case r.Method == "POST":
					creates++
					metadata, uploaded := readMultipart(t, r)
					if metadata.Id != "reserved" || !bytes.Equal(uploaded, data) {
						t.Errorf("retry did not reuse ID and rewind data: %q %q", metadata.Id, uploaded)
					}
					if creates == 1 {
						return nil, io.ErrUnexpectedEOF
					}
					return response(409, `{"error":{"code":409}}`), nil
				case r.Method == "GET" && r.URL.Path == "/drive/v3/files/reserved":
					metadata := map[string]any{"id": "reserved", "name": "name", "mimeType": "text/plain", "parents": []string{"folder"}, "size": fmt.Sprint(len(data)), "md5Checksum": checksum}
					switch mismatch {
					case "parent":
						metadata["parents"] = []string{"wrong"}
					case "name":
						metadata["name"] = "wrong"
					case "mime":
						metadata["mimeType"] = folderMIME
					case "size":
						metadata["size"] = "0"
					case "md5":
						delete(metadata, "md5Checksum")
					case "trashed":
						metadata["trashed"] = true
					case "shared":
						metadata["driveId"] = "shared"
					case "id":
						metadata["id"] = "wrong"
					}
					return jsonResponse(metadata), nil
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					return response(500, `{}`), nil
				}
			})
			result, err := client.Upload(context.Background(), "folder", model.SourceFile{Name: "name", MIMEType: "text/plain"}, file, checksum)
			if mismatch == "" {
				if err != nil || result.ID != "reserved" {
					t.Fatalf("got %+v %v", result, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "does not match") {
				t.Fatalf("conflicting %s was not rejected: %v", mismatch, err)
			}
			if creates != 2 || reserved != 1 {
				t.Errorf("unsafe retry: creates=%d reserved=%d", creates, reserved)
			}
		})
	}
}

func TestLargeResumableUploadUsesBoundedChunksAndRetries(t *testing.T) {
	file, _ := localFile(t, nil)
	size := int64(2*uploadChunk + 17)
	if err := file.Truncate(size); err != nil {
		t.Fatal(err)
	}
	hash := md5.New()
	if _, err := io.Copy(hash, file); err != nil {
		t.Fatal(err)
	}
	checksum := hex.EncodeToString(hash.Sum(nil))
	creates, chunks := 0, 0
	var uploaded int64
	receivedHash := md5.New()
	client := sdkClient(t, func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/drive/v3/files/generateIds":
			return response(200, `{"ids":["large"]}`), nil
		case r.Method == "POST" && r.URL.Path == "/upload/drive/v3/files":
			creates++
			if r.URL.Query().Get("uploadType") != "resumable" {
				t.Errorf("large file must be resumable, got %q", r.URL.Query().Get("uploadType"))
			}
			var metadata drive.File
			if err := json.NewDecoder(r.Body).Decode(&metadata); err != nil || metadata.Id != "large" {
				t.Errorf("metadata: %+v %v", metadata, err)
			}
			res := response(200, `{}`)
			res.Header.Set("Location", "https://upload.invalid/session?secret=DO_NOT_LOG")
			return res, nil
		case r.Method == "POST" && r.URL.Path == "/session":
			chunks++
			if chunks == 1 {
				_, _ = io.Copy(io.Discard, r.Body)
				res := response(429, `{"error":{"code":429,"message":"DO_NOT_LOG"}}`)
				res.Header.Set("Retry-After", "1")
				return res, nil
			}
			n, err := io.Copy(receivedHash, io.LimitReader(r.Body, uploadChunk+1))
			if err != nil || n > uploadChunk {
				t.Errorf("unbounded chunk: %d %v", n, err)
			}
			total := "*"
			if uploaded+n == size {
				total = fmt.Sprint(size)
			}
			expectedRange := fmt.Sprintf("bytes %d-%d/%s", uploaded, uploaded+n-1, total)
			if r.Header.Get("Content-Range") != expectedRange {
				t.Errorf("got range %q, expected %q", r.Header.Get("Content-Range"), expectedRange)
			}
			uploaded += n
			if uploaded < size {
				res := response(200, "")
				res.Header.Set("X-Http-Status-Code-Override", "308")
				res.Header.Set("Range", fmt.Sprintf("bytes=0-%d", uploaded-1))
				return res, nil
			}
			return jsonResponse(map[string]any{"id": "large", "size": fmt.Sprint(size), "md5Checksum": checksum}), nil
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			return response(500, `{}`), nil
		}
	})
	result, err := client.Upload(context.Background(), "folder", model.SourceFile{Name: "large.bin"}, file, checksum)
	if err != nil || result.Size == nil || *result.Size != size || result.MD5 != checksum {
		t.Fatalf("got %+v %v", result, err)
	}
	if creates != 1 || chunks != 4 || uploaded != size || hex.EncodeToString(receivedHash.Sum(nil)) != checksum {
		t.Fatalf("create=%d chunks=%d uploaded=%d", creates, chunks, uploaded)
	}
	if _, err := file.Stat(); err != nil {
		t.Fatal("large upload closed caller's file")
	}
}

func TestUploadPermanent403AndUnknownSize(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
		status  int
		wantErr bool
	}{
		{"permission failure", `{"error":{"code":403,"message":"PRIVATE_TOKEN https://upload.invalid/private","errors":[{"reason":"insufficientFilePermissions"}]}}`, 403, true},
		{"unknown size", `{"id":"reserved"}`, 200, false},
		{"wrong returned ID", `{"id":"different"}`, 200, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			file, checksum := localFile(t, []byte("x"))
			creates := 0
			client := sdkClient(t, func(r *http.Request) (*http.Response, error) {
				if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/generateIds") {
					return response(200, `{"ids":["reserved"]}`), nil
				}
				if r.Method != "POST" {
					t.Errorf("unexpected request: %s", r.Method)
				}
				creates++
				_, _ = io.Copy(io.Discard, r.Body)
				return response(test.status, test.payload), nil
			})
			result, err := client.Upload(context.Background(), "folder", model.SourceFile{Name: "name"}, file, checksum)
			if (err != nil) != test.wantErr || creates != 1 {
				t.Fatalf("got %+v err=%v creates=%d", result, err, creates)
			}
			if err != nil && (strings.Contains(err.Error(), "PRIVATE_TOKEN") || strings.Contains(err.Error(), "https://")) {
				t.Fatalf("sensitive upload error: %v", err)
			}
			if !test.wantErr && result.Size != nil {
				t.Errorf("missing size should be unknown: %+v", result)
			}
			if _, err := file.Stat(); err != nil {
				t.Fatal("upload closed the file on failure")
			}
		})
	}
}

func TestResumableFailureIsBoundedAndLeavesFileOpen(t *testing.T) {
	for _, failure := range []string{"cancellation", "permanent 403", "long throttle"} {
		t.Run(failure, func(t *testing.T) {
			file, checksum := localFile(t, nil)
			if err := file.Truncate(uploadChunk + 1); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			creates, chunks := 0, 0
			client := sdkClient(t, func(r *http.Request) (*http.Response, error) {
				switch {
				case r.URL.Path == "/drive/v3/files/generateIds":
					return response(200, `{"ids":["reserved"]}`), nil
				case r.URL.Path == "/upload/drive/v3/files":
					creates++
					res := response(200, `{}`)
					res.Header.Set("Location", "https://private.invalid/session")
					return res, nil
				case r.URL.Path == "/session":
					chunks++
					if failure == "cancellation" {
						cancel()
						return nil, r.Context().Err()
					}
					if failure == "permanent 403" {
						return response(403, `{"error":{"code":403,"message":"PRIVATE_RESPONSE","errors":[{"reason":"storageQuotaExceeded"}]}}`), nil
					}
					res := response(429, `{"error":{"code":429,"message":"PRIVATE_RESPONSE"}}`)
					res.Header.Set("Retry-After", "3600")
					return res, nil
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					return response(500, `{}`), nil
				}
			})
			_, err := client.Upload(ctx, "folder", model.SourceFile{Name: "large"}, file, checksum)
			if err == nil || creates != 1 || chunks != 1 {
				t.Fatalf("unexpected upload retry: creates=%d chunks=%d err=%v", creates, chunks, err)
			}
			if failure == "cancellation" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation not preserved: %v", err)
			}
			assertNoSecrets(t, err.Error())
			if _, err := file.Stat(); err != nil {
				t.Fatal("failed upload closed caller's file")
			}
		})
	}
}

func TestEmptyUploadAndMissingSizeMetadata(t *testing.T) {
	for _, sizeField := range []bool{false, true} {
		file, checksum := localFile(t, nil)
		client := sdkClient(t, func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/drive/v3/files/generateIds" {
				return response(200, `{"ids":["empty"]}`), nil
			}
			_, data := readMultipart(t, r)
			if len(data) != 0 {
				t.Error("empty upload gained content")
			}
			metadata := map[string]any{"id": "empty", "md5Checksum": checksum}
			if sizeField {
				metadata["size"] = "0"
			}
			return jsonResponse(metadata), nil
		})
		result, err := client.Upload(context.Background(), "folder", model.SourceFile{Name: "empty"}, file, checksum)
		if err != nil || result.Size == nil || *result.Size != 0 || result.MD5 != checksum {
			t.Fatalf("empty upload: %+v %v", result, err)
		}
	}
	file, checksum := localFile(t, []byte("content"))
	client := sdkClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/drive/v3/files/generateIds" {
			return response(200, `{"ids":["file"]}`), nil
		}
		_, _ = io.Copy(io.Discard, r.Body)
		return jsonResponse(map[string]any{"id": "file", "md5Checksum": checksum}), nil
	})
	result, err := client.Upload(context.Background(), "folder", model.SourceFile{Name: "file"}, file, checksum)
	if err != nil || result.Size != nil || result.MD5 != checksum {
		t.Fatalf("missing size was fabricated: %+v %v", result, err)
	}
}

func TestReadRetriesPreserveSDKPagination(t *testing.T) {
	attempts := 0
	client := sdkClient(t, func(r *http.Request) (*http.Response, error) {
		attempts++
		if r.URL.Query().Get("pageToken") != "" {
			t.Error("failed read advanced pagination")
		}
		if attempts == 1 {
			return response(403, `{"error":{"code":403,"errors":[{"reason":"userRateLimitExceeded"}]}}`), nil
		}
		return response(200, `{"files":[{"id":"1","name":"kept"}]}`), nil
	})
	names, err := client.ExistingNames(context.Background(), "folder")
	if err != nil || attempts != 2 || len(names) != 1 {
		t.Fatalf("SDK transient read: %+v attempts=%d err=%v", names, attempts, err)
	}
}
