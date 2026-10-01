package torque

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTaskListResponseShapes(t *testing.T) {
	tests := []struct {
		name, body string
		count      int
		more       bool
	}{
		{"legacy empty", `{"tasks":[],"total":0}`, 0, false},
		{"legacy null empty", `{"tasks":null}`, 0, false},
		{"legacy last page", `{"tasks":[{"id":"T1"},{"id":"T2"}],"has_more":false}`, 2, false},
		{"legacy probe", `{"tasks":[{"id":"T1"},{"id":"T2"},{"id":"T3"}],"has_more":false}`, 2, true},
		{"legacy server continuation", `{"tasks":[{"id":"T1"}],"has_more":true}`, 1, true},
		{"items empty", `{"items":[],"meta":{"returned":0,"has_more":false,"next_cursor":null}}`, 0, false},
		{"items last page", `{"items":[{"id":"T1"},{"id":"T2"}],"meta":{"has_more":false}}`, 2, false},
		{"items probe on server last page", `{"items":[{"id":"T1"},{"id":"T2"},{"id":"T3"}],"meta":{"has_more":false}}`, 2, true},
		{"items server continuation", `{"items":[{"id":"T1"}],"meta":{"has_more":true,"next_cursor":"next"}}`, 1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if r.URL.Path != "/api/v1/tasks" || q.Get("limit") != "3" || q.Has("include_total") {
					t.Errorf("unexpected query: %s", r.URL)
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			got, err := NewClient(server.URL).ListTasks(context.Background(), ListFilters{Limit: 2})
			if err != nil {
				t.Fatalf("ListTasks: %v", err)
			}
			if len(got.Tasks) != tc.count || got.More != tc.more {
				t.Fatalf("page = %+v; want count=%d more=%v", got, tc.count, tc.more)
			}
			if tc.count > 0 && got.Tasks[0].ID != "T1" {
				t.Fatalf("wrong decoded task: %+v", got.Tasks[0])
			}
		})
	}
}

func TestTaskListMalformedResponsesFail(t *testing.T) {
	for _, body := range []string{
		`{`, `null`, `[]`, `{}`, `{"error":"bad"}`, `{"tasks":{}}`, `{"tasks":"bad"}`, `{"tasks":[{"priority":"bad"}]}`,
		`{"items":null,"meta":{"has_more":false}}`, `{"items":{},"meta":{"has_more":false}}`, `{"items":[]}`,
		`{"items":[],"meta":null}`, `{"items":[],"meta":{}}`, `{"items":[],"meta":{"has_more":"true"}}`, `{"items":[],"tasks":[],"meta":{"has_more":false}}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			_, err := NewClient(server.URL).ListTasks(context.Background(), ListFilters{Limit: 2})
			if !errors.Is(err, ErrTorqueUnavailable) {
				t.Fatalf("error = %v; want unusable Torque response", err)
			}
		})
	}
}
