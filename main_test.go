package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func sessionCookieForRole(t *testing.T, role string) *http.Cookie {
	t.Helper()

	token := fmt.Sprintf("test-session-%d", time.Now().UnixNano())
	sessionsMu.Lock()
	sessions[token] = session{UserID: 1, Role: role, Expires: time.Now().Add(time.Hour)}
	sessionsMu.Unlock()
	t.Cleanup(func() {
		sessionsMu.Lock()
		delete(sessions, token)
		sessionsMu.Unlock()
	})
	return &http.Cookie{Name: sessionCookie, Value: token}
}

func TestHandleIndexRoutesByRole(t *testing.T) {
	tests := []struct {
		name     string
		role     string
		wantPath string
	}{
		{name: "admin", role: "admin", wantPath: "/admin"},
		{name: "customer", role: "customer", wantPath: "/customer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.AddCookie(sessionCookieForRole(t, tt.role))
			rec := httptest.NewRecorder()
			handleIndex(rec, req)

			if rec.Code != http.StatusSeeOther {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
			}
			if got := rec.Header().Get("Location"); got != tt.wantPath {
				t.Errorf("Location = %q, want %q", got, tt.wantPath)
			}
		})
	}
}

func TestCustomerPageRejectsAdmin(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/customer", nil)
	req.AddCookie(sessionCookieForRole(t, "admin"))
	rec := httptest.NewRecorder()
	handleHome(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}
