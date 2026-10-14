// Package pingdom is a minimal client for the checks endpoints of the Pingdom API 3.1.
package pingdom

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

const DefaultBaseURL = "https://api.pingdom.com/api/3.1"

// CheckRequest is the desired state of a check. Pingdom leaves omitted fields unchanged on update,
// so UpdateCheck sends explicit empty values for clearable fields (see body).
type CheckRequest struct {
	Name                     string   `json:"name,omitempty"`
	Host                     string   `json:"host,omitempty"`
	Type                     string   `json:"type,omitempty"`
	Paused                   bool     `json:"paused"`
	Resolution               *int32   `json:"resolution,omitempty"`
	UserIDs                  string   `json:"userids,omitempty"`
	TeamIDs                  string   `json:"teamids,omitempty"`
	IntegrationIDs           []int64  `json:"integrationids,omitempty"`
	SendNotificationWhenDown *int32   `json:"sendnotificationwhendown,omitempty"`
	NotifyAgainEvery         *int32   `json:"notifyagainevery,omitempty"`
	NotifyWhenBackUp         *bool    `json:"notifywhenbackup,omitempty"`
	Tags                     []string `json:"tags,omitempty"`
	ProbeFilters             []string `json:"probe_filters,omitempty"`
	IPv6                     *bool    `json:"ipv6,omitempty"`
	ResponseTimeThreshold    *int32   `json:"responsetime_threshold,omitempty"`
	CustomMessage            string   `json:"custom_message,omitempty"`
	Port                     *int32   `json:"port,omitempty"`
	Encryption               *bool    `json:"encryption,omitempty"`
	URL                      string   `json:"url,omitempty"`
	ShouldContain            string   `json:"shouldcontain,omitempty"`
	ShouldNotContain         string   `json:"shouldnotcontain,omitempty"`
	PostData                 string   `json:"postdata,omitempty"`
	RequestHeaders           []string `json:"requestheaders,omitempty"`
	VerifyCertificate        *bool    `json:"verify_certificate,omitempty"`
	SSLDownDaysBefore        *int32   `json:"ssl_down_days_before,omitempty"`
	AdditionalURLs           string   `json:"additionalurls,omitempty"`
	StringToSend             string   `json:"stringtosend,omitempty"`
	StringToExpect           string   `json:"stringtoexpect,omitempty"`
	Nameserver               string   `json:"nameserver,omitempty"`
	ExpectedIP               string   `json:"expectedip,omitempty"`
	Auth                     string   `json:"auth,omitempty"`
}

// Error is a non-2xx response from Pingdom.
type Error struct {
	StatusCode int
	Message    string
	// RetryAfter is set on 429 responses.
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return fmt.Sprintf("pingdom API returned %d: %s", e.StatusCode, e.Message)
}

// NotFound reports a missing check. Pingdom answers 403 "Invalid check identifier" rather than 404 for them.
func (e *Error) NotFound() bool {
	return e.StatusCode == http.StatusNotFound ||
		(e.StatusCode == http.StatusForbidden && e.Message == "Invalid check identifier")
}

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func New(token string) *Client {
	return &Client{BaseURL: DefaultBaseURL, Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// CreateCheck creates a check and returns its ID.
func (c *Client) CreateCheck(ctx context.Context, req *CheckRequest) (int64, error) {
	var out struct {
		Check struct {
			ID int64 `json:"id"`
		} `json:"check"`
	}
	body, err := req.body(false)
	if err != nil {
		return 0, err
	}
	if err := c.do(ctx, http.MethodPost, "/checks", body, &out); err != nil {
		return 0, err
	}
	return out.Check.ID, nil
}

func (c *Client) UpdateCheck(ctx context.Context, id int64, req *CheckRequest) error {
	body, err := req.body(true)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/checks/%d", id), body, nil)
}

// body builds the JSON body. Pingdom folds a multi-entry requestheaders array into a single header, so
// headers are sent as requestheaders, requestheaders1, ... with one entry each.
// On update the type is dropped (it cannot change) and clearable fields get explicit empty values;
// the http-only ones are skipped for other types because Pingdom rejects them with a 400.
func (r *CheckRequest) body(update bool) (map[string]any, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	delete(m, "requestheaders")
	for i, h := range r.RequestHeaders {
		k := "requestheaders"
		if i > 0 {
			k += strconv.Itoa(i)
		}
		m[k] = []string{h}
	}
	if !update {
		return m, nil
	}
	delete(m, "type")
	empty := func(v any, keys ...string) {
		for _, k := range keys {
			if _, ok := m[k]; !ok {
				m[k] = v
			}
		}
	}
	empty("", "custom_message", "userids", "teamids")
	empty([]string{}, "tags", "probe_filters", "integrationids")
	if r.Type == "http" {
		// shortcut: shouldcontain and shouldnotcontain are not cleared, Pingdom rejects both together with a 400.
		empty("", "postdata")
		empty([]string{}, "requestheaders")
	}
	return m, nil
}

func (c *Client) DeleteCheck(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("/checks/%d", id), nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body bytes.Buffer
	if in != nil {
		if err := json.NewEncoder(&body).Encode(in); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		perr := &Error{StatusCode: resp.StatusCode, Message: http.StatusText(resp.StatusCode)}
		var e struct {
			Error struct {
				ErrorMessage string `json:"errormessage"`
			} `json:"error"`
		}
		if json.NewDecoder(resp.Body).Decode(&e) == nil && e.Error.ErrorMessage != "" {
			perr.Message = e.Error.ErrorMessage
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			perr.RetryAfter = retryAfter(resp.Header)
		}
		return perr
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// retryAfter reads "Remaining: 0 Time until reset: 3589" from Req-Limit-Short/Long, whichever is exhausted.
func retryAfter(h http.Header) time.Duration {
	wait := time.Minute
	for _, k := range []string{"Req-Limit-Short", "Req-Limit-Long"} {
		var remaining, reset int
		if _, err := fmt.Sscanf(h.Get(k), "Remaining: %d Time until reset: %d", &remaining, &reset); err == nil && remaining == 0 {
			wait = max(wait, time.Duration(reset)*time.Second)
		}
	}
	return wait
}
