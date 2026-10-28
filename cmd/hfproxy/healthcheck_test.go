package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthcheckURL(t *testing.T) {
	for listen, want := range map[string]string{
		"":               "http://127.0.0.1:8080/healthz",
		"0.0.0.0:9000":   "http://127.0.0.1:9000/healthz",
		":9000":          "http://127.0.0.1:9000/healthz",
		"[::]:9000":      "http://127.0.0.1:9000/healthz",
		"10.1.2.3:8081":  "http://10.1.2.3:8081/healthz",
		"not-an-address": "http://127.0.0.1:8080/healthz",
	} {
		if got := healthcheckURL(listen); got != want {
			t.Errorf("healthcheckURL(%q) = %q, want %q", listen, got, want)
		}
	}
}

func TestProbeHealthReportsStatus(t *testing.T) {
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
	defer server.Close()
	if err := probeHealth(server.URL); err != nil {
		t.Fatalf("healthy server: %v", err)
	}
	status = http.StatusServiceUnavailable
	if err := probeHealth(server.URL); err == nil {
		t.Fatal("an unhealthy server must fail the probe")
	}
	server.Close()
	if err := probeHealth(server.URL); err == nil {
		t.Fatal("an unreachable server must fail the probe")
	}
}
