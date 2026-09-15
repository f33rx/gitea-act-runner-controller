package gitea

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// JobLogSize must talk to the client's base URL, not the host Gitea embeds in job.url
// (its ROOT_URL, unreachable from inside the cluster in dev).
func TestJobLogSizeUsesClientBaseURL(t *testing.T) {
	const path = "/api/v1/repos/org/repo/actions/jobs/7"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path+"/logs" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "token tok" {
			t.Errorf("missing token header")
		}
		w.Header().Set("Content-Length", "1234")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	size, err := c.JobLogSize("http://localhost:3000" + path)
	if err != nil {
		t.Fatalf("JobLogSize: %v", err)
	}
	if size != 1234 {
		t.Fatalf("size = %d, want 1234", size)
	}
}

// A reply whose length the transport cannot determine (chunked, because the handler
// flushes before writing) must be an error, not a size. Returning -1 here would read as
// "the log did not grow" and stall detection would kill the job at the stall window.
func TestJobLogSizeRejectsUnknownLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Flushing commits the headers before the body is known, forcing chunked
		// transfer-encoding and leaving Content-Length unset.
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("some streamed log output"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	if _, err := c.JobLogSize(srv.URL + "/api/v1/repos/o/r/actions/jobs/1"); err == nil {
		t.Fatal("expected an error for a response with no determinable length")
	}
}

// The request must opt out of transparent compression, otherwise the transport
// advertises gzip on our behalf and strips the Content-Length we depend on.
func TestJobLogSizeRequestsIdentityEncoding(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	if _, err := c.JobLogSize(srv.URL + "/api/v1/repos/o/r/actions/jobs/1"); err != nil {
		t.Fatalf("JobLogSize: %v", err)
	}
	if got != "identity" {
		t.Fatalf("Accept-Encoding = %q, want identity", got)
	}
}
