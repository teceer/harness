package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeDiffboard records what reaches diffboard through the proxy.
func fakeDiffboard(t *testing.T) (*[]*http.Request, func()) {
	t.Helper()
	var got []*http.Request
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Clone(r.Context()))
		io.WriteString(w, "diffboard:"+r.URL.Path)
	}))
	t.Setenv("HARNESS_DIFFBOARD_URL", up.URL)
	return &got, up.Close
}

func TestDiffboardProxyNeedsTheToken(t *testing.T) {
	got, stop := fakeDiffboard(t)
	defer stop()
	_, mux := testServer(t)

	for _, path := range []string{"/diffboard/design", "/diffboard/api/designs", "/diffboard/api/d/x/ops"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a token: %d, want 401", path, rec.Code)
		}
	}
	if len(*got) != 0 {
		t.Fatalf("an unauthenticated request reached diffboard: %v", (*got)[0].URL)
	}
}

func TestDiffboardProxyOnlyExposesDesignBoards(t *testing.T) {
	got, stop := fakeDiffboard(t)
	defer stop()
	_, mux := testServer(t)

	for _, path := range []string{"/diffboard/api/worktree", "/diffboard/api/open", "/diffboard/api/b/default/state", "/diffboard/app.js", "/diffboard/"} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer secret-token")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Errorf("%s went through (%d)", path, rec.Code)
		}
	}
	if len(*got) != 0 {
		t.Fatalf("diffboard got %v", (*got)[0].URL)
	}
}

func TestDiffboardProxyForwardsWithoutCredentials(t *testing.T) {
	got, stop := fakeDiffboard(t)
	defer stop()
	_, mux := testServer(t)

	req := httptest.NewRequest("POST", "/diffboard/api/d/rooms/ops?x=1", strings.NewReader(`{"ops":[]}`))
	req.Host = "mac.tailnet.ts.net"
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "diffboard:/api/d/rooms/ops" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	r := (*got)[0]
	if r.Header.Get("Authorization") != "" || r.URL.Query().Has("t") {
		t.Error("the harness token leaked to diffboard")
	}
	if r.Header.Get("X-Forwarded-Host") != "mac.tailnet.ts.net" {
		t.Errorf("X-Forwarded-Host = %q: diffboard needs it to check the Origin", r.Header.Get("X-Forwarded-Host"))
	}
}

func TestDiffboardProxyFirstVisitSetsACookieAndHidesTheToken(t *testing.T) {
	got, stop := fakeDiffboard(t)
	defer stop()
	_, mux := testServer(t)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/diffboard/design?t=secret-token", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/diffboard/design" {
		t.Fatalf("first visit: %d → %q, want a redirect without the token", rec.Code, rec.Header().Get("Location"))
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != diffboardCookie || !cookies[0].HttpOnly {
		t.Fatalf("cookie: %+v", cookies)
	}

	// the page's own requests carry only the cookie
	req := httptest.NewRequest("GET", "/diffboard/design.js", nil)
	req.AddCookie(cookies[0])
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "diffboard:/design.js" {
		t.Fatalf("with the cookie: %d %q", rec.Code, rec.Body.String())
	}
	if (*got)[0].Header.Get("Cookie") != "" {
		t.Error("the cookie leaked to diffboard")
	}

	bad := httptest.NewRequest("GET", "/diffboard/design.js", nil)
	bad.AddCookie(&http.Cookie{Name: diffboardCookie, Value: "guess"})
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, bad)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a wrong cookie: %d", rec.Code)
	}
}

func TestDiffboardProxyWhenDiffboardIsDown(t *testing.T) {
	t.Setenv("HARNESS_DIFFBOARD_URL", "http://127.0.0.1:1")
	_, mux := testServer(t)
	req := httptest.NewRequest("GET", "/diffboard/api/designs", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "diffboard start") {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}
