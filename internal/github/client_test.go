package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient returns a Client pointed at a test server plus the mux to
// add endpoint handlers to. The token endpoint is pre-wired and counts mints.
func newTestClient(t *testing.T) (*Client, *http.ServeMux, *atomic.Int32) {
	t.Helper()
	mux := http.NewServeMux()
	var mints atomic.Int32
	mux.HandleFunc("POST /app/installations/7/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		mints.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      fmt.Sprintf("itok-%d", mints.Load()),
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := New(srv.URL, 42, 7, testKey(t))
	return c, mux, &mints
}

func TestInstallationTokenCached(t *testing.T) {
	c, mux, mints := newTestClient(t)
	mux.HandleFunc("GET /orgs/o/actions/runners/5", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer itok-1" {
			t.Errorf("Authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(Runner{ID: 5, Status: "online"})
	})
	ctx := context.Background()
	for range 3 {
		if _, err := c.GetRunner(ctx, "orgs/o", 5); err != nil {
			t.Fatal(err)
		}
	}
	if mints.Load() != 1 {
		t.Errorf("token minted %d times, want 1", mints.Load())
	}
}

func TestInstallationTokenRefreshAfterExpiry(t *testing.T) {
	c, mux, mints := newTestClient(t)
	mux.HandleFunc("GET /orgs/o/actions/runners/5", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Runner{ID: 5})
	})
	ctx := context.Background()
	if _, err := c.GetRunner(ctx, "orgs/o", 5); err != nil {
		t.Fatal(err)
	}
	c.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, err := c.GetRunner(ctx, "orgs/o", 5); err != nil {
		t.Fatal(err)
	}
	if mints.Load() != 2 {
		t.Errorf("token minted %d times, want 2", mints.Load())
	}
}

func TestGenerateJITConfig(t *testing.T) {
	c, mux, _ := newTestClient(t)
	mux.HandleFunc("POST /repos/o/r/actions/runners/generate-jitconfig", func(w http.ResponseWriter, r *http.Request) {
		var req JITRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Name != "ghq-fmt-abc" || req.RunnerGroupID != 1 || len(req.Labels) != 2 {
			t.Errorf("unexpected request: %+v", req)
		}
		_ = json.NewEncoder(w).Encode(JITResult{
			Runner:           Runner{ID: 99, Name: req.Name},
			EncodedJITConfig: "blob123",
		})
	})
	got, err := c.GenerateJITConfig(context.Background(), "repos/o/r", JITRequest{
		Name: "ghq-fmt-abc", RunnerGroupID: 1, Labels: []string{"self-hosted", "fmt"}, WorkFolder: "_work",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Runner.ID != 99 || got.EncodedJITConfig != "blob123" {
		t.Errorf("got %+v", got)
	}
}

func TestDeleteRunnerNotFound(t *testing.T) {
	c, mux, _ := newTestClient(t)
	mux.HandleFunc("DELETE /orgs/o/actions/runners/5", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	err := c.DeleteRunner(context.Background(), "orgs/o", 5)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestListRunnersPaginates(t *testing.T) {
	c, mux, _ := newTestClient(t)
	mux.HandleFunc("GET /orgs/o/actions/runners", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		var runners []Runner
		if page == "1" {
			for i := range 100 {
				runners = append(runners, Runner{ID: int64(i)})
			}
		} else {
			runners = []Runner{{ID: 100}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"runners": runners})
	})
	got, err := c.ListRunners(context.Background(), "orgs/o")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 101 {
		t.Errorf("got %d runners, want 101", len(got))
	}
}

func TestRunnerGroupID(t *testing.T) {
	c, mux, _ := newTestClient(t)
	mux.HandleFunc("GET /orgs/o/actions/runner-groups", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"runner_groups": []map[string]any{
				{"id": 1, "name": "Default"},
				{"id": 5, "name": "heavy"},
			},
		})
	})
	id, err := c.RunnerGroupID(context.Background(), "orgs/o", "heavy")
	if err != nil {
		t.Fatal(err)
	}
	if id != 5 {
		t.Errorf("id = %d", id)
	}
	if _, err := c.RunnerGroupID(context.Background(), "orgs/o", "absent"); err == nil {
		t.Error("want error for unknown group")
	}
	// repo scope never calls the API
	id, err = c.RunnerGroupID(context.Background(), "repos/o/r", "Default")
	if err != nil || id != 1 {
		t.Errorf("repo scope: id=%d err=%v", id, err)
	}
}
