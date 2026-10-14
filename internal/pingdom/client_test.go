package pingdom

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClient(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody map[string]any
	status := http.StatusOK
	var respBody string
	var respHeader http.Header

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		gotBody = nil
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		maps.Copy(w.Header(), respHeader)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "tok", HTTP: srv.Client()}
	ctx := context.Background()

	respBody = `{"check":{"id":42,"name":"x"}}`
	id, err := c.CreateCheck(ctx, &CheckRequest{Name: "x", Host: "example.com", Type: "http"})
	if err != nil || id != 42 {
		t.Fatalf("create: id=%d err=%v", id, err)
	}
	if gotMethod != "POST" || gotPath != "/checks" || gotAuth != "Bearer tok" || gotBody["type"] != "http" {
		t.Fatalf("create sent %s %s %q %v", gotMethod, gotPath, gotAuth, gotBody)
	}
	if v, ok := gotBody["paused"]; !ok || v != false {
		t.Fatalf("paused must always be sent, got %v", gotBody)
	}

	if err := c.UpdateCheck(ctx, 42, &CheckRequest{Host: "b.com"}); err != nil || gotMethod != "PUT" || gotPath != "/checks/42" {
		t.Fatalf("update: %s %s %v", gotMethod, gotPath, err)
	}
	if err := c.DeleteCheck(ctx, 42); err != nil || gotMethod != "DELETE" || gotPath != "/checks/42" {
		t.Fatalf("delete: %s %s %v", gotMethod, gotPath, err)
	}

	status, respBody = http.StatusBadRequest, `{"error":{"statuscode":400,"errormessage":"Invalid host"}}`
	var perr *Error
	if err := c.UpdateCheck(ctx, 42, &CheckRequest{}); !errors.As(err, &perr) || perr.StatusCode != 400 || perr.Message != "Invalid host" {
		t.Fatalf("400: %v", err)
	}

	status, respBody = http.StatusNotFound, ``
	if err := c.DeleteCheck(ctx, 1); !errors.As(err, &perr) || perr.StatusCode != 404 {
		t.Fatalf("404: %v", err)
	}

	status, respBody = http.StatusForbidden, `{"error":{"statuscode":403,"errormessage":"Invalid check identifier"}}`
	if err := c.DeleteCheck(ctx, 1); !errors.As(err, &perr) || !perr.NotFound() {
		t.Fatalf("403 invalid check identifier should be NotFound: %v", err)
	}
	respBody = `{"error":{"statuscode":403,"errormessage":"Insufficient permissions"}}`
	if err := c.DeleteCheck(ctx, 1); !errors.As(err, &perr) || perr.NotFound() {
		t.Fatalf("other 403s must not be NotFound: %v", err)
	}

	status = http.StatusTooManyRequests
	respHeader = http.Header{"Req-Limit-Short": {"Remaining: 0 Time until reset: 3589"}}
	if err := c.DeleteCheck(ctx, 1); !errors.As(err, &perr) || perr.RetryAfter != 3589*time.Second {
		t.Fatalf("429: %v retryAfter=%v", err, perr)
	}
}
