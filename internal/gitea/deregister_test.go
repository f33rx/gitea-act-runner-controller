package gitea

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// An already-absent registration is the outcome every caller wants, so it is not an
// error; anything else unexpected must surface with its status.
func TestDeregisterOrgRunnerTreatsAbsentAsSuccess(t *testing.T) {
	for _, tc := range []struct {
		status  int
		wantErr bool
	}{
		{http.StatusNoContent, false},
		{http.StatusNotFound, false},
		{http.StatusForbidden, true},
		{http.StatusInternalServerError, true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/orgs/acme/actions/runners/7" {
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(tc.status)
		}))
		err := NewClient(srv.URL, "t").DeregisterOrgRunner(context.Background(), "acme", 7)
		srv.Close()
		if (err != nil) != tc.wantErr {
			t.Errorf("status %d: err=%v, wantErr=%v", tc.status, err, tc.wantErr)
		}
	}
}
