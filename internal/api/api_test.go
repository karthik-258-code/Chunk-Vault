package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/api"
	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/vault"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := vault.Init(dir); err != nil {
		t.Fatal(err)
	}
	v, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	return httptest.NewServer(api.NewRouter(v))
}

func TestPutGetRoundTrip(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	body := "hello chunkvault API"

	// Put
	resp, err := http.Post(ts.URL+"/v1/files/test.txt", "application/octet-stream", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT status = %d, body = %s", resp.StatusCode, b)
	}

	// Get
	resp, err = http.Get(ts.URL + "/v1/files/test.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("GET body = %q, want %q", got, body)
	}
}

func TestGetMissing(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/files/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET missing: status = %d, want 404", resp.StatusCode)
	}
}

func TestDeleteFile(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	// Put first
	resp, err := http.Post(ts.URL+"/v1/files/del.txt", "application/octet-stream", strings.NewReader("to delete"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Delete
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/files/del.txt", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d", resp.StatusCode)
	}

	// Get should 404
	resp, err = http.Get(ts.URL + "/v1/files/del.txt")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after DELETE: status = %d", resp.StatusCode)
	}
}

func TestListFiles(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	// Empty list
	resp, err := http.Get(ts.URL + "/v1/files")
	if err != nil {
		t.Fatal(err)
	}
	var entries []vault.Entry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(entries) != 0 {
		t.Fatalf("expected empty list, got %d", len(entries))
	}

	// Put two files
	for _, name := range []string{"a.txt", "b.txt"} {
		resp, err = http.Post(ts.URL+"/v1/files/"+name, "application/octet-stream", strings.NewReader("content of "+name))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	resp, err = http.Get(ts.URL + "/v1/files")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(entries) != 2 {
		t.Fatalf("expected 2 files, got %d", len(entries))
	}
}

func TestStats(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats status = %d", resp.StatusCode)
	}
	var s vault.Stats
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	if s.Files != 0 {
		t.Fatalf("expected 0 files, got %d", s.Files)
	}
}

func TestGC(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/gc?dry_run=true", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gc status = %d", resp.StatusCode)
	}
	var gc vault.GCStats
	if err := json.NewDecoder(resp.Body).Decode(&gc); err != nil {
		t.Fatal(err)
	}
}

func TestVerify(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	// Put a file so there's something to verify
	resp, err := http.Post(ts.URL+"/v1/files/v.txt", "application/octet-stream", strings.NewReader("verify me"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	resp, err = http.Post(ts.URL+"/v1/verify?full=true", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("verify status = %d", resp.StatusCode)
	}
	var rep vault.VerifyReport
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("verify found problems: %+v", rep.Problems)
	}
}

func TestPassphraseInQueryParamRejected(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/files/test.txt?passphrase=secret")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("passphrase in query: status = %d, want 400", resp.StatusCode)
	}
}

func TestSlashedFileName(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	body := "nested file content"
	resp, err := http.Post(ts.URL+"/v1/files/dir/sub/file.txt", "application/octet-stream", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT slashed: status = %d", resp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/v1/files/dir/sub/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Fatalf("GET slashed = %q, want %q", got, body)
	}
}

