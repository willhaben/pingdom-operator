package pingdom

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

const typeHTTP = "http"

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
	id, err := c.CreateCheck(ctx, &CheckRequest{Name: "x", Host: "example.com", Type: typeHTTP})
	if err != nil || id != 42 {
		t.Fatalf("create: id=%d err=%v", id, err)
	}
	if gotMethod != "POST" || gotPath != "/checks" || gotAuth != "Bearer tok" || gotBody["type"] != typeHTTP {
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

func TestBody(t *testing.T) {
	headers := []string{"A:1", "B:2", "C:3"}
	httpReq := &CheckRequest{Host: "h", Type: typeHTTP, RequestHeaders: headers}

	m, err := httpReq.body(false)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"requestheaders": []string{"A:1"}, "requestheaders1": []string{"B:2"}, "requestheaders2": []string{"C:3"}}
	for k, v := range want {
		if !reflect.DeepEqual(m[k], v) {
			t.Errorf("create %s = %v, want %v", k, m[k], v)
		}
	}
	if m["type"] != typeHTTP {
		t.Errorf("create must send the type, got %v", m["type"])
	}
	for _, k := range []string{"postdata", "tags", "teamids"} {
		if _, ok := m[k]; ok {
			t.Errorf("create must not send empty %s", k)
		}
	}

	m, _ = httpReq.body(true)
	if _, ok := m["type"]; ok {
		t.Error("update must not send the type")
	}
	for k, v := range want {
		if !reflect.DeepEqual(m[k], v) {
			t.Errorf("update %s = %v, want %v", k, m[k], v)
		}
	}
	if m["postdata"] != "" || !reflect.DeepEqual(m["tags"], []string{}) || m["teamids"] != "" {
		t.Errorf("update must clear unset fields, got %v", m)
	}

	m, _ = (&CheckRequest{Type: typeHTTP, PostData: "x", Tags: []string{"t"}}).body(true)
	if m["postdata"] != "x" || !reflect.DeepEqual(m["tags"], []any{"t"}) || !reflect.DeepEqual(m["requestheaders"], []string{}) {
		t.Errorf("update must keep set fields and clear headers, got %v", m)
	}

	m, _ = (&CheckRequest{Type: "dns"}).body(true)
	for _, k := range []string{"postdata", "requestheaders"} {
		if _, ok := m[k]; ok {
			t.Errorf("update of a dns check must not send http-only %s (Pingdom answers 400)", k)
		}
	}
	if !reflect.DeepEqual(m["integrationids"], []string{}) || m["custom_message"] != "" {
		t.Errorf("update of a dns check must clear common fields, got %v", m)
	}
}
