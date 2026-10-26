package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBillingNotEnabled(t *testing.T) {
	rec := httptest.NewRecorder()
	billingNotEnabled(rec, httptest.NewRequest(http.MethodPost, "/admin/api/billing/checkout", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "billing_not_enabled") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}
