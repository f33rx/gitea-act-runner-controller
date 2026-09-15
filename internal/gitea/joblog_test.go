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
