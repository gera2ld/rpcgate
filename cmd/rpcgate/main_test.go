package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRoute(t *testing.T) {
	tag := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("X-To", name) })
	}
	h := route(tag("gw"), tag("pb"))
	for path, want := range map[string]string{
		"/Xk3abcdefghijklmnopqrstuvwx/bsc":    "gw",
		"/Xk3abcdefghijklmnopqrstuvwx/status": "gw",
		"/short/bsc":                          "gw", // the gateway answers 401
		"/":                                   "pb",
		"/_/":                                 "pb",
		"/_/images/logo.svg":                  "pb",
		"/api/health":                         "pb",
		"/api/collections/tokens/records":     "pb",
		"/favicon.ico":                        "pb",
		"/dashboard":                          "pb",
		"/token/bsc/extra":                    "pb",
		"/token/":                             "pb",
		"//bsc":                               "pb",
		"/api/backups":                        "403",
		"/api/backups/pb_backup.zip":          "403",
		"/api/backups/x/restore":              "403",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		got := rec.Header().Get("X-To")
		if rec.Code == http.StatusForbidden {
			got = "403"
		}
		if got != want {
			t.Errorf("%s -> %s, want %s", path, got, want)
		}
	}
}
