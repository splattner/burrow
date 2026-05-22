package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/splattner/burrow/internal/config"
	"github.com/splattner/burrow/internal/logging"
)

func TestHandleMetricsExposesCurrentCounters(t *testing.T) {
	s := New(config.Config{Namespace: "default"}, logging.NoOp())

	s.metrics.IncSessions()
	s.metrics.IncStreams()
	s.metrics.AddStaleServicesDeleted(3)
	s.metrics.IncStreamBackpressureDrops()

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()

	s.handleMetrics(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}
	body := rr.Body.String()

	checks := []string{
		"burrow_sessions_active 1",
		"burrow_streams_active 1",
		"burrow_stale_services_deleted_total 3",
		"burrow_stream_backpressure_drops_total 1",
	}
	for _, needle := range checks {
		if !strings.Contains(body, needle) {
			t.Fatalf("expected metrics output to contain %q, got:\n%s", needle, body)
		}
	}
}
