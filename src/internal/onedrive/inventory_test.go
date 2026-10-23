package onedrive

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

const rootJSON = `{"id":"root","name":"root","folder":{},"root":{}}`

func TestInventoryCompleteDeltaSDKPagination(t *testing.T) {
	next := "https://graph.microsoft.com/v1.0/drives/drive/root/delta?$skiptoken=a%2Bb%2Fc&custom=%2F"
	var requests []string
	client := testClient(t, func(request *http.Request) (*http.Response, error) {
		requests = append(requests, request.URL.String())
		switch {
		case request.URL.Path == "/v1.0/drives/drive/root":
			return jsonResponse(request, 200, rootJSON), nil
		case request.URL.RawQuery == "":
			body := `{"value":[` + rootJSON + `,
				{"id":"child","name":"old.txt","file":{},"size":1,"parentReference":{"id":"folder"}},
				{"id":"removed","name":"removed","file":{},"parentReference":{"id":"root"}},
				{"id":"folder","name":"z-folder","folder":{},"parentReference":{"id":"root"}}],
				"@odata.nextLink":` + marshalJSON(t, next) + `}`
			return jsonResponse(request, 200, body), nil
		case request.URL.String() == next:
			return jsonResponse(request, 200, `{"value":[
				{"id":"child","name":"new.txt","file":{},"size":9,"parentReference":{"id":"folder"}},
				{"id":"removed","deleted":{}},
				{"id":"remote","name":"a-remote","remoteItem":{"id":"elsewhere"},"parentReference":{"id":"root"}},
				{"id":"package","name":"b-package","package":{"type":"oneNote"},"parentReference":{"id":"root"}},
				{"id":"unknown","name":"c-unknown","parentReference":{"id":"root"}}],
				"@odata.deltaLink":"https://graph.microsoft.com/v1.0/drives/drive/root/delta?token=done"}`), nil
		default:
			t.Fatalf("unexpected pagination request %s", request.URL.String())
			return nil, errors.New("unexpected request")
		}
	})
	items, err := client.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var paths, types []string
	for _, item := range items {
		paths = append(paths, item.Path)
		types = append(types, item.Type)
		if item.ID == "child" && (item.Size == nil || *item.Size != 9) {
			t.Fatalf("last delta occurrence not applied: %#v", item)
		}
		if item.ID == "unknown" && item.Size != nil {
			t.Fatal("missing size is not nil")
		}
	}
	if !reflect.DeepEqual(paths, []string{"/a-remote", "/b-package", "/c-unknown", "/z-folder", "/z-folder/new.txt"}) {
		t.Fatalf("unexpected paths %#v", paths)
	}
	if !reflect.DeepEqual(types, []string{"remote", "package", "unknown", "folder", "file"}) {
		t.Fatalf("unexpected types %#v", types)
	}
	if len(requests) != 3 || requests[1] != "https://graph.microsoft.com/v1.0/drives/drive/root/delta" {
		t.Fatalf("did not enumerate a fresh delta: %#v", requests)
	}
}

func TestInventoryInvalidGraphData(t *testing.T) {
	for _, test := range []struct {
		name   string
		value  string
		suffix string
		want   string
	}{
		{"missing ID", `[{"name":"file"}]`, "", "without an ID"},
		{"missing name", `[{"id":"x","parentReference":{"id":"root"}}]`, "", "invalid name"},
		{"missing parent", `[{"id":"x","name":"file"}]`, "", "no parent"},
		{"orphan", `[{"id":"x","name":"file","parentReference":{"id":"absent"}}]`, "", "orphan"},
		{"cycle", `[{"id":"x","name":"x","parentReference":{"id":"y"}},{"id":"y","name":"y","parentReference":{"id":"x"}}]`, "", "cycle"},
		{"self cycle", `[{"id":"x","name":"x","parentReference":{"id":"x"}}]`, "", "cycle"},
		{"foreign parent", `[{"id":"x","name":"x","parentReference":{"id":"root","driveId":"elsewhere"}}]`, "", "another drive"},
		{"second root", `[{"id":"x","name":"x","root":{},"parentReference":{"id":"root"}}]`, "", "unexpected root"},
		{"root missing facet", `[{"id":"root","name":"root"}]`, "", "root facet"},
		{"deleted root", `[{"id":"root","deleted":{}}]`, "", "root was deleted"},
		{"invalid path name", `[{"id":"x","name":"../x","parentReference":{"id":"root"}}]`, "", "invalid name"},
		{"incomplete", `[]`, `"other":"field"`, "incomplete"},
		{"bad next URL", `[]`, `"@odata.nextLink":"https://evil.example/secret"`, "continuation"},
		{"HTTP next URL", `[]`, `"@odata.nextLink":"http://graph.microsoft.com/v1.0/drives/drive/root/delta?secret"`, "continuation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := testClient(t, func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == "/v1.0/drives/drive/root" {
					return jsonResponse(request, 200, rootJSON), nil
				}
				suffix := test.suffix
				if suffix == "" {
					suffix = `"@odata.deltaLink":"https://graph.microsoft.com/done"`
				}
				return jsonResponse(request, 200, `{"value":`+test.value+`,`+suffix+`}`), nil
			})
			items, err := client.Inventory(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) || items != nil {
				t.Fatalf("items=%#v, error=%v, want %q", items, err, test.want)
			}
		})
	}
}

func TestInventoryPartialFailureAndRepeatedPage(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"partial failure", 503, `{"error":{"code":"unavailable","message":"SECRET"}}`},
		{"repeated page", 200, `{"value":[],"@odata.nextLink":"https://graph.microsoft.com/page2?token=private"}`},
		{"nil response", 204, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := testClient(t, func(request *http.Request) (*http.Response, error) {
				switch request.URL.Path {
				case "/v1.0/drives/drive/root":
					return jsonResponse(request, 200, rootJSON), nil
				case "/v1.0/drives/drive/root/delta":
					return jsonResponse(request, 200, `{"value":[{"id":"x","name":"x","parentReference":{"id":"root"}}],"@odata.nextLink":"https://graph.microsoft.com/page2?token=private"}`), nil
				default:
					return jsonResponse(request, test.status, test.body), nil
				}
			})
			items, err := client.Inventory(context.Background())
			if err == nil || items != nil || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("partial result or unsafe error: %#v, %v", items, err)
			}
		})
	}
}

func TestInventoryResynchronizesOnlyOnce(t *testing.T) {
	for _, alwaysGone := range []bool{false, true} {
		t.Run(map[bool]string{false: "recovers", true: "bounded"}[alwaysGone], func(t *testing.T) {
			roots, deltas := 0, 0
			client := testClient(t, func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == "/v1.0/drives/drive/root" {
					roots++
					return jsonResponse(request, 200, rootJSON), nil
				}
				deltas++
				if deltas == 1 || alwaysGone {
					return jsonResponse(request, 410, `{"error":{"code":"resyncRequired","message":"start again"}}`), nil
				}
				if request.URL.RawQuery != "" {
					t.Fatal("restart reused a token")
				}
				return jsonResponse(request, 200, `{"value":[],"@odata.deltaLink":"https://graph.microsoft.com/done"}`), nil
			})
			_, err := client.Inventory(context.Background())
			if (err != nil) != alwaysGone || roots != 2 || deltas != 2 {
				t.Fatalf("error=%v roots=%d deltas=%d", err, roots, deltas)
			}
		})
	}
}

func TestInventoryLastOccurrenceCanRestoreDeletedItem(t *testing.T) {
	client := testClient(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/v1.0/drives/drive/root" {
			return jsonResponse(request, 200, rootJSON), nil
		}
		return jsonResponse(request, 200, `{"value":[{"id":"x","deleted":{}},{"id":"x","name":"restored","file":{},"parentReference":{"id":"root"}}],"@odata.deltaLink":"https://graph.microsoft.com/done"}`), nil
	})
	items, err := client.Inventory(context.Background())
	if err != nil || len(items) != 1 || items[0].Path != "/restored" {
		t.Fatalf("items=%#v, error=%v", items, err)
	}
}

func TestInventoryContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := testClient(t, func(request *http.Request) (*http.Response, error) {
		return nil, request.Context().Err()
	})
	if items, err := client.Inventory(ctx); !errors.Is(err, context.Canceled) || items != nil {
		t.Fatalf("items=%#v, error=%v", items, err)
	}
}
