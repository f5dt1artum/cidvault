package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGatewayThroughRealListener(t *testing.T) {
	h := Handler()
	ts := httptest.NewServer(h)
	defer ts.Close()

	fileCID := uploadBytes(t, h, []byte("payload"))
	subCID := uploadBytes(t, h, marshalDirectory(t, []dirEntrySpec{
		{name: "leaf", typ: "file", cid: fileCID},
	}))
	root := uploadBytes(t, h, marshalDirectory(t, []dirEntrySpec{
		{name: "%2F-name", typ: "file", cid: fileCID},
		{name: "sub", typ: "directory", cid: subCID},
	}))

	get := func(rawPath string) (int, string, string) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+rawPath, nil)
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatalf("roundtrip %s: %v", rawPath, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
	}

	if code, ct, body := get("/v1/gateway/" + root + "/%252F-name"); code != 200 || ct != "application/octet-stream" || body != "payload" {
		t.Fatalf("double-encoded name: %d %q %q", code, ct, body)
	}
	if code, _, _ := get("/v1/gateway/" + root + "/a%2Fb"); code != 400 {
		t.Fatalf("a%%2Fb code = %d, want 400", code)
	}
	if code, _, _ := get("/v1/gateway/" + root + "/sub%2Fleaf"); code != 400 {
		t.Fatalf("sub%%2Fleaf code = %d, want 400", code)
	}
	resp, err := http.Get(ts.URL + "/v1/gateway/" + root + "/a/../b")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("raw .. code = %d, want 400 (Location=%q)", resp.StatusCode, resp.Header.Get("Location"))
	}
	if code, ct, _ := get("/v1/gateway/" + root); code != 200 || !strings.HasPrefix(ct, "application/vnd.cidvault.directory") {
		t.Fatalf("root dir: %d %q", code, ct)
	}
	if code, _, body := get("/v1/gateway/" + root + "/sub/leaf"); code != 200 || body != "payload" {
		t.Fatalf("nested file: %d %q", code, body)
	}
	if code, ct, _ := get("/v1/gateway/" + root + "/sub"); code != 200 || !strings.HasPrefix(ct, "application/vnd.cidvault.directory") {
		t.Fatalf("subdir: %d %q", code, ct)
	}
	hp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	hp.Body.Close()
	if hp.StatusCode != 200 {
		t.Fatalf("healthz = %d", hp.StatusCode)
	}
}
