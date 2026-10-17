package dash

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLogoHandler(t *testing.T) {
	const name = "Chain & Test"
	data := []byte("cached PNG")
	handler := LogoHandler(func(chain string) []byte {
		if chain == name {
			return data
		}
		return nil
	})
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest(method, "/logo?chain=Chain%20%26%20Test", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d", method, w.Code)
		}
		for header, want := range map[string]string{
			"Content-Type":            "image/png",
			"X-Content-Type-Options":  "nosniff",
			"Content-Security-Policy": "default-src 'none'",
			"Cache-Control":           "public, max-age=86400",
			"Content-Length":          "10",
		} {
			if got := w.Header().Get(header); got != want {
				t.Errorf("%s: %s = %q, want %q", method, header, got, want)
			}
		}
		want := string(data)
		if method == http.MethodHead {
			want = ""
		}
		if w.Body.String() != want {
			t.Fatalf("%s: body = %q", method, w.Body.String())
		}
	}
	for _, path := range []string{"/logo", "/logo?chain=unknown"} {
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d", path, w.Code)
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest(method, "/logo?chain=Chain%20%26%20Test", nil))
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
			t.Fatalf("%s: status = %d, Allow = %q", method, w.Code, w.Header().Get("Allow"))
		}
	}
	w := httptest.NewRecorder()
	LogoHandler(nil)(w, httptest.NewRequest(http.MethodGet, "/logo?chain=unknown", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("nil provider: status = %d", w.Code)
	}
}
