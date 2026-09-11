package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/notify"
)

type nopSender struct{}

func (nopSender) SendResourceNotification(context.Context, string) error { return nil }
func (nopSender) SendSamplingNotification(context.Context, string, any, notify.Event, string) error {
	return nil
}

func newNotifyTestMux(t *testing.T) (*http.ServeMux, *notify.Manager, string) {
	t.Helper()
	mgr := notify.NewManager(nopSender{})
	rule, err := mgr.Register("sess-1", "shell-1", notify.ChannelResource, notify.EventOutput, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Register("sess-2", "shell-2", notify.ChannelSampling, notify.EventExit, 0, nil); err != nil {
		t.Fatal(err)
	}
	h := &Handler{NotifyMgr: mgr}
	mux := http.NewServeMux()
	h.Register(mux)
	return mux, mgr, rule.ID
}

func decodeNotifyBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("bad JSON %q: %v", rr.Body.String(), err)
	}
	return m
}

func TestNotificationsAPI_ListFilterAndDelete(t *testing.T) {
	mux, mgr, ruleID := newNotifyTestMux(t)

	// All rules.
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/notifications", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d", rr.Code)
	}
	all, _ := decodeNotifyBody(t, rr)["notifications"].([]any)
	if len(all) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(all))
	}

	// session_id filter.
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/notifications?session_id=sess-1", nil))
	filtered, _ := decodeNotifyBody(t, rr)["notifications"].([]any)
	if len(filtered) != 1 {
		t.Fatalf("expected 1 rule for sess-1, got %d", len(filtered))
	}
	if got := filtered[0].(map[string]any)["shell_id"]; got != "shell-1" {
		t.Fatalf("expected shell-1, got %v", got)
	}

	// Delete.
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/notifications/"+ruleID, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("delete status = %d body=%s", rr.Code, rr.Body.String())
	}
	if len(mgr.List("")) != 1 {
		t.Fatalf("expected 1 rule left after delete, got %d", len(mgr.List("")))
	}

	// Deleting again is a 404.
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/notifications/"+ruleID, nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing rule, got %d", rr.Code)
	}
}

func TestNotificationsAPI_NilManagerIsSafe(t *testing.T) {
	h := &Handler{}
	mux := http.NewServeMux()
	h.Register(mux)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/notifications", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d", rr.Code)
	}
	if got, _ := decodeNotifyBody(t, rr)["notifications"].([]any); len(got) != 0 {
		t.Fatalf("expected empty list, got %v", got)
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/notifications/x", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when manager absent, got %d", rr.Code)
	}
}
