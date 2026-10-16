package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestControlAuthentication(t *testing.T) {
	handler := Authenticated("secret", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for _, tc := range []struct {
		token, origin string
		want          int
	}{{"", "", 401}, {"wrong", "", 401}, {"secret", "https://evil.invalid", 403}, {"secret", "", 204}} {
		r := httptest.NewRequest("GET", "http://local/status", nil)
		if tc.token != "" {
			r.Header.Set("Authorization", "Bearer "+tc.token)
		}
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("got %d want %d", w.Code, tc.want)
		}
	}
}
