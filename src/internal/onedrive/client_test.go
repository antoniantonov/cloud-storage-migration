package onedrive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/microsoft/kiota-abstractions-go/authentication"
	kiotahttp "github.com/microsoft/kiota-http-go"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func jsonResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       request,
	}
}

func sdkClient(t *testing.T, handle roundTripFunc) *msgraphsdk.GraphServiceClient {
	t.Helper()
	transport := kiotahttp.NewCustomTransportWithParentTransport(handle,
		kiotahttp.NewUrlReplaceHandler(true, map[string]string{"/users/me-token-to-replace": "/me"}))
	adapter, err := msgraphsdk.NewGraphRequestAdapterWithParseNodeFactoryAndSerializationWriterFactoryAndHttpClient(
		&authentication.AnonymousAuthenticationProvider{}, nil, nil, &http.Client{Transport: transport},
	)
	if err != nil {
		t.Fatal(err)
	}
	return msgraphsdk.NewGraphServiceClient(adapter)
}

func testClient(t *testing.T, handle roundTripFunc) *Client {
	t.Helper()
	graph := sdkClient(t, func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet {
			t.Fatalf("unexpected Graph method %s", request.Method)
		}
		if request.URL.Host != "graph.microsoft.com" {
			t.Fatalf("unexpected Graph host %s", request.URL.Host)
		}
		if request.URL.Path == "/v1.0/me/drive" {
			return jsonResponse(request, 200, `{"id":"drive","driveType":"personal"}`), nil
		}
		return handle(request)
	})
	client, err := newClient(context.Background(), graph, downloadClient())
	if err != nil {
		t.Fatal(err)
	}
	client.retryDelay = 0
	return client
}

func workDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", ".onedrive-test-")
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

func marshalJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestNewClientRequiresPersonalDrive(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		ok   bool
	}{
		{"personal", `{"id":"drive","driveType":"personal"}`, true},
		{"business", `{"id":"drive","driveType":"business"}`, false},
		{"library", `{"id":"drive","driveType":"documentLibrary"}`, false},
		{"missing type", `{"id":"drive"}`, false},
		{"missing id", `{"driveType":"personal"}`, false},
		{"empty", `{}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			graph := sdkClient(t, func(request *http.Request) (*http.Response, error) {
				if request.URL.Path != "/v1.0/me/drive" {
					t.Fatalf("unexpected request %s", request.URL.Path)
				}
				return jsonResponse(request, 200, test.body), nil
			})
			_, err := newClient(context.Background(), graph, nil)
			if (err == nil) != test.ok {
				t.Fatalf("newClient error = %v, want success %v", err, test.ok)
			}
		})
	}
}

func TestFileEligibilityThroughSDK(t *testing.T) {
	for _, test := range []struct {
		name  string
		facet string
		ok    bool
	}{
		{"file", `"file":{}`, true},
		{"own shared file", `"file":{"mimeType":"image/png"},"shared":{}`, true},
		{"folder", `"folder":{}`, false},
		{"folder and file", `"folder":{},"file":{}`, false},
		{"package and file", `"package":{"type":"oneNote"},"file":{}`, false},
		{"remote and file", `"remoteItem":{"id":"other"},"file":{}`, false},
		{"deleted file", `"deleted":{},"file":{}`, false},
		{"root file", `"root":{},"file":{}`, false},
		{"unknown", `"description":"unknown"`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := testClient(t, func(request *http.Request) (*http.Response, error) {
				if request.URL.Path != "/v1.0/drives/drive/items/opaque!1" {
					t.Fatalf("not an ID lookup: %s", request.URL.Path)
				}
				body := fmt.Sprintf(`{"id":"opaque!1","name":"photo.png","size":5,"parentReference":{"id":"root","driveId":"drive"},"@microsoft.graph.downloadUrl":"https://download.example/content?secret=yes",%s}`, test.facet)
				return jsonResponse(request, 200, body), nil
			})
			file, err := client.File(context.Background(), "opaque!1")
			if (err == nil) != test.ok {
				t.Fatalf("File error = %v, want success %v", err, test.ok)
			}
			if test.ok && (file.ID != "opaque!1" || file.Name != "photo.png" || file.Size == nil || *file.Size != 5) {
				t.Fatalf("unexpected file metadata %#v", file)
			}
			if test.ok && test.name == "file" && file.MIMEType != "application/octet-stream" {
				t.Fatal("missing MIME type did not default to binary")
			}
		})
	}
}

func TestFileInvalidMetadata(t *testing.T) {
	for _, fragment := range []string{
		`"id":"different","name":"file","file":{}`,
		`"id":"item","file":{}`,
		`"id":"item","name":"file","file":{},"size":-1`,
		`"id":"item","name":"file","file":{},"parentReference":{"driveId":"other"}`,
		`"id":"item","name":"file","file":{},"@microsoft.graph.downloadUrl":"http://insecure.example/secret"`,
		`"id":"item","name":"file","file":{},"@microsoft.graph.downloadUrl":123`,
	} {
		t.Run(fragment, func(t *testing.T) {
			client := testClient(t, func(request *http.Request) (*http.Response, error) {
				return jsonResponse(request, 200, "{"+fragment+"}"), nil
			})
			if _, err := client.File(context.Background(), "item"); err == nil {
				t.Fatal("accepted invalid metadata")
			}
		})
	}
}

func TestFileOpaqueIDEncodedAndErrorsSanitized(t *testing.T) {
	client := testClient(t, func(request *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(request.URL.EscapedPath(), "/opaque%3Fid") || request.URL.RawQuery != "" {
			t.Fatalf("opaque ID was interpreted as URL syntax: %s", request.URL.EscapedPath())
		}
		return jsonResponse(request, 404, `{"error":{"code":"itemNotFound","message":"SECRET_DOWNLOAD_URL"}}`), nil
	})
	if _, err := client.File(context.Background(), "opaque?id"); err == nil || !strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("unexpected error %v", err)
	}
}
