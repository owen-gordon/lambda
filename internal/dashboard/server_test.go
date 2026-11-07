package dashboard

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const testToken = "dashboard-token-0123456789abcdef0123456789"

func TestPageRequiresExactlyOneQueryToken(t *testing.T) {
	handler, err := handler(sha256.Sum256([]byte(testToken)), "https://api.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/?token=wrong", "/?token=", "/?token=" + testToken + "&token=" + testToken, "/?token=%zz"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+testToken)
		r.AddCookie(&http.Cookie{Name: "session", Value: testToken})
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 401 || strings.Contains(w.Body.String(), "<html") {
			t.Fatalf("unauthorized page leaked: %s %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/?token="+url.QueryEscape(testToken), nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Lambda console") || strings.Contains(w.Body.String(), "__SCRIPT__") {
		t.Fatal("page not served")
	}
	if strings.Contains(w.Body.String(), testToken) {
		t.Fatal("server reflected the access token into HTML")
	}
	if w.Header().Get("Referrer-Policy") != "no-referrer" || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "'sha256-") {
		t.Fatal("missing page protections")
	}
}

func TestHealthDoesNotAuthorizePage(t *testing.T) {
	handler, err := handler(sha256.Sum256([]byte(testToken)), "https://api.example")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/healthz", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("authenticated health failed")
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 401 {
		t.Fatal("anonymous health accepted")
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("HEAD", "/?token="+testToken, nil))
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatal("HEAD returned a page body")
	}
}
