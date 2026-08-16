package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aurelialedger/internal/database/postgres"
)

func TestPublicHealthAndAuthentication(t *testing.T) {
	api := New(nil, "secret", func(context.Context) error { return nil })
	health := httptest.NewRecorder()
	api.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health = %d", health.Code)
	}
	for _, header := range []string{"", "Bearer wrong"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/v1/owners/1/balance", nil)
		if header != "" {
			request.Header.Set("Authorization", header)
		}
		api.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%q status = %d", header, recorder.Code)
		}
	}
}

func TestHealthReportsDatabaseFailure(t *testing.T) {
	api := New(nil, "secret", func(context.Context) error { return errors.New("unavailable") })
	recorder := httptest.NewRecorder()
	api.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("health = %d", recorder.Code)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	want := postgres.PageCursor{CreatedAt: time.Date(2026, 8, 12, 1, 2, 3, 0, time.UTC), ID: 42}
	got, err := decodeCursor(encodeCursor(want))
	if err != nil || got.ID != want.ID || !got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("got %#v, %v", got, err)
	}
	if _, err := decodeCursor("not-base64"); err == nil {
		t.Fatal("expected malformed cursor error")
	}
}
