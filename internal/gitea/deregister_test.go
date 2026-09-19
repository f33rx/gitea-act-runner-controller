package gitea

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An already-absent registration is the outcome every caller wants, so it is not an
// error; anything else unexpected must surface with its status and Gitea's reason.
func TestDeregisterOrgRunnerTreatsAbsentAsSuccess(t *testing.T) {
	for _, tc := range []struct {
		status   int
		body     string
		wantErr  bool
		wantText string
	}{
		{http.StatusNoContent, "", false, ""},
		{http.StatusNotFound, `{"message":"not found"}`, false, ""},
		{http.StatusForbidden, `{"message":"token lacks write:organization"}`, true, "write:organization"},
		{http.StatusInternalServerError, "", true, "status 500"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/orgs/acme/actions/runners/7" {
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		err := NewClient(srv.URL, "t").DeregisterOrgRunner(context.Background(), "acme", 7)
		srv.Close()
		if (err != nil) != tc.wantErr {
			t.Errorf("status %d: err=%v, wantErr=%v", tc.status, err, tc.wantErr)
		}
		if err != nil && !strings.Contains(err.Error(), tc.wantText) {
			t.Errorf("status %d: error %q should carry %q", tc.status, err, tc.wantText)
		}
	}
}
