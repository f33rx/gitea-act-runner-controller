package gitea

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// Gitea caps pages below the requested limit, so every page up to total_count must be
// read; all requested statuses go in one query.
func TestListOrgJobsReadsEveryPage(t *testing.T) {
	const total, serverPage = 70, 30
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query()["status"]; len(got) != 2 || got[0] != "queued" || got[1] != "in_progress" {
			t.Errorf("status params = %v, want [queued in_progress]", got)
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		var jobs []Job
		for id := (page-1)*serverPage + 1; id <= page*serverPage && id <= total; id++ {
			jobs = append(jobs, Job{ID: int64(id), Status: "queued"})
		}
		// A job shifted from the previous page shows up again and must not double count.
		if page == 2 {
			jobs = append(jobs, Job{ID: 1, Status: "queued"})
		}
		_ = json.NewEncoder(w).Encode(ListOrgJobsResponse{Jobs: jobs, TotalCount: total})
	}))
	defer srv.Close()

	jobs, gotTotal, err := NewClient(srv.URL, "t").ListOrgJobs(context.Background(), "org", "queued", "in_progress")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != total || gotTotal != total {
		t.Fatalf("jobs = %d, total = %d, want %d", len(jobs), gotTotal, total)
	}
}

func TestListOrgJobsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"message":"token does not have required scope"}`)
	}))
	defer srv.Close()
	if _, _, err := NewClient(srv.URL, "t").ListOrgJobs(context.Background(), "org", "queued"); err == nil {
		t.Fatal("want error on 403")
	}
}
