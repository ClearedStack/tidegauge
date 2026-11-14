// Package muster implements Tidegauge's HTTP client for Muster contract v1
// and an in-memory stand-in Muster server for standalone tests.
//
// The client speaks only the five worker calls defined in CONTRACT.md:
// POST /v1/lease, /v1/heartbeat, /v1/complete, /v1/fail and /v1/needs_subtool.
package muster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ClearedStack/tidegauge/internal/watch"
)

// Contract constants from ClearedStack/muster contract/v1/CONTRACT.md.
const (
	ContractVersion = "v1"
	MaxBodyBytes    = 64 << 10 // 64 KiB maximum worker request/response body

	DefaultTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"

	PathLease        = "/v1/lease"
	PathHeartbeat    = "/v1/heartbeat"
	PathComplete     = "/v1/complete"
	PathFail         = "/v1/fail"
	PathNeedsSubtool = "/v1/needs_subtool"
)

// Grant is Muster's 200 answer to POST /v1/lease when work is available.
type Grant struct {
	Lease    string                     `json:"lease"`
	Kind     string                     `json:"kind"`
	ID       string                     `json:"id"`
	State    string                     `json:"state"`
	Lane     string                     `json:"lane"`
	Attempt  int                        `json:"attempt"`
	Expires  string                     `json:"expires"`
	Found    *FoundView                 `json:"found,omitempty"`
	Results  map[string]json.RawMessage `json:"results,omitempty"`
	Proposal *ProposalView              `json:"proposal,omitempty"`
	Incident *IncidentView              `json:"incident,omitempty"`
}

// FoundView is where an item was found when recorded through POST /v1/items.
type FoundView struct {
	Source  string `json:"source"`
	Title   string `json:"title"`
	Address string `json:"address"`
}

// ProposalView describes an edit proposal step.
type ProposalView struct {
	Target     string `json:"target"`
	Repository string `json:"repository"`
	File       string `json:"file"`
	EditKind   string `json:"edit_kind"`
	Change     string `json:"change"`
	Reason     string `json:"reason"`
}

// IncidentView describes an incident step for Bellbuoy.
type IncidentView struct {
	ID     string `json:"id"`
	Worker string `json:"worker"`
	Code   string `json:"code"`
	State  string `json:"state"`
}

// Beat is Muster's 200 answer to POST /v1/heartbeat.
type Beat struct {
	Lease   string `json:"lease"`
	Expires string `json:"expires"`
}

// Ack is Muster's 200 answer to POST /v1/complete, /v1/fail and /v1/needs_subtool.
type Ack struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Next  string `json:"next,omitempty"`
	Held  bool   `json:"held,omitempty"`
}

// Refusal is Muster's exact three-field refusal payload for HTTP status >= 400.
type Refusal struct {
	Status int    `json:"-"`
	Error  string `json:"error"`
	Cause  string `json:"cause"`
	Reason string `json:"reason"`
}

// RefusalError wraps Refusal to satisfy the standard error interface cleanly
// while keeping Refusal's JSON field name "Error" exact.
type RefusalError struct {
	Refusal
}

func (e *RefusalError) Error() string {
	return fmt.Sprintf("muster refused call: status=%d code=%s cause=%s", e.Status, e.Refusal.Error, e.Cause)
}

// UnwrapRefusal returns the underlying *RefusalError when err represents a
// contract refusal from Muster.
func UnwrapRefusal(err error) (*RefusalError, bool) {
	var re *RefusalError
	if errors.As(err, &re) {
		return re, true
	}
	return nil, false
}

// TransportError represents a sanitized network, timeout or protocol error
// talking to Muster without leaking internal URLs, hostnames or addresses.
type TransportError struct {
	Code    string
	Message string
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("muster transport (%s): %s", e.Code, e.Message)
}

// ClientError represents an invalid caller argument rejected before sending a
// request that would violate CONTRACT.md.
type ClientError struct {
	Message string
}

func (e *ClientError) Error() string { return "muster client: " + e.Message }

// IsRetryable reports whether a failed Muster call should remain in the local
// spool and be retried after a backoff.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	var ce *ClientError
	if errors.As(err, &ce) {
		return false
	}
	if re, ok := UnwrapRefusal(err); ok {
		switch re.Status {
		case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		default:
			return false
		}
	}
	var te *TransportError
	return errors.As(err, &te)
}

// TokenSource supplies the Kubernetes ServiceAccount bearer token for each request.
type TokenSource func() (string, error)

// StaticTokenSource returns a fixed token string, primarily for unit tests.
func StaticTokenSource(token string) TokenSource {
	trimmed := strings.TrimSpace(token)
	return func() (string, error) {
		if trimmed == "" {
			return "", &ClientError{Message: "empty bearer token"}
		}
		return trimmed, nil
	}
}

// FileTokenSource reads the Kubernetes ServiceAccount token from disk on each
// call so projected token rotation works without restarting the pod.
func FileTokenSource(path string) TokenSource {
	if strings.TrimSpace(path) == "" {
		path = DefaultTokenFile
	}
	return func() (string, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", &ClientError{Message: "cannot read serviceaccount token"}
		}
		tok := strings.TrimSpace(string(b))
		if tok == "" {
			return "", &ClientError{Message: "serviceaccount token file is empty"}
		}
		return tok, nil
	}
}

// Client calls Muster's five worker endpoints over cluster-private HTTP.
type Client struct {
	baseURL string
	token   TokenSource
	http    *http.Client
}

// NewClient constructs a strict Contract v1 worker client.
func NewClient(baseURL string, token TokenSource, httpClient *http.Client) (*Client, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return nil, &ClientError{Message: "missing Muster base URL"}
	}
	if token == nil {
		token = FileTokenSource(DefaultTokenFile)
	}
	hc := httpClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	} else {
		copied := *hc
		hc = &copied
	}
	// Never follow redirects so the ServiceAccount bearer token cannot be forwarded.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{
		baseURL: trimmed,
		token:   token,
		http:    hc,
	}, nil
}

type leaseReq struct {
	Contract string `json:"contract"`
}

type heartbeatReq struct {
	Contract string `json:"contract"`
	Lease    string `json:"lease"`
}

type completeReq struct {
	Contract string          `json:"contract"`
	Lease    string          `json:"lease"`
	State    string          `json:"state"`
	Result   json.RawMessage `json:"result,omitempty"`
}

type failReq struct {
	Contract string `json:"contract"`
	Lease    string `json:"lease"`
	Code     string `json:"code"`
}

type needsSubtoolReq struct {
	Contract string `json:"contract"`
	Lease    string `json:"lease"`
}

// Lease asks Muster for Tidegauge's next queue step. It returns (nil, nil) when
// Muster answers 204 No Content.
func (c *Client) Lease(ctx context.Context) (*Grant, error) {
	status, body, err := c.post(ctx, PathLease, leaseReq{Contract: ContractVersion})
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent {
		if len(bytes.TrimSpace(body)) != 0 {
			return nil, &TransportError{Code: watch.CodeParse, Message: "204 lease response carried a body"}
		}
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, &TransportError{Code: watch.CodeParse, Message: "unexpected HTTP status on lease"}
	}
	var g Grant
	if err := decodeStrict(body, &g); err != nil {
		return nil, err
	}
	if g.Lease == "" || g.Kind == "" || g.ID == "" || g.State == "" {
		return nil, &TransportError{Code: watch.CodeParse, Message: "lease grant missing required fields"}
	}
	return &g, nil
}

// Heartbeat extends an active lease.
func (c *Client) Heartbeat(ctx context.Context, lease string) (*Beat, error) {
	if strings.TrimSpace(lease) == "" {
		return nil, &ClientError{Message: "lease is required"}
	}
	status, body, err := c.post(ctx, PathHeartbeat, heartbeatReq{
		Contract: ContractVersion,
		Lease:    lease,
	})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &TransportError{Code: watch.CodeParse, Message: "unexpected HTTP status on heartbeat"}
	}
	var b Beat
	if err := decodeStrict(body, &b); err != nil {
		return nil, err
	}
	if b.Lease == "" || b.Expires == "" {
		return nil, &TransportError{Code: watch.CodeParse, Message: "heartbeat response missing required fields"}
	}
	return &b, nil
}

// Complete finishes a leased step with a target state and an optional JSON
// object result.
func (c *Client) Complete(ctx context.Context, lease, state string, result any) (*Ack, error) {
	if strings.TrimSpace(lease) == "" {
		return nil, &ClientError{Message: "lease is required"}
	}
	if strings.TrimSpace(state) == "" {
		return nil, &ClientError{Message: "state is required"}
	}
	var rawResult json.RawMessage
	if result != nil {
		encoded, err := marshalJSONObject(result)
		if err != nil {
			return nil, err
		}
		rawResult = encoded
	}
	status, body, err := c.post(ctx, PathComplete, completeReq{
		Contract: ContractVersion,
		Lease:    lease,
		State:    state,
		Result:   rawResult,
	})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &TransportError{Code: watch.CodeParse, Message: "unexpected HTTP status on complete"}
	}
	var ack Ack
	if err := decodeStrict(body, &ack); err != nil {
		return nil, err
	}
	if ack.ID == "" || ack.State == "" {
		return nil, &TransportError{Code: watch.CodeParse, Message: "complete response missing required fields"}
	}
	return &ack, nil
}

// Fail reports E_TIMEOUT or E_PARSE for a held lease. Per CONTRACT.md, missing
// sub-tool rules must call NeedsSubtool rather than Fail.
func (c *Client) Fail(ctx context.Context, lease, code string) (*Ack, error) {
	if strings.TrimSpace(lease) == "" {
		return nil, &ClientError{Message: "lease is required"}
	}
	if code != watch.CodeTimeout && code != watch.CodeParse {
		return nil, &ClientError{Message: "fail code must be E_TIMEOUT or E_PARSE; use NeedsSubtool for E_NEEDS_SUBTOOL"}
	}
	status, body, err := c.post(ctx, PathFail, failReq{
		Contract: ContractVersion,
		Lease:    lease,
		Code:     code,
	})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &TransportError{Code: watch.CodeParse, Message: "unexpected HTTP status on fail"}
	}
	var ack Ack
	if err := decodeStrict(body, &ack); err != nil {
		return nil, err
	}
	if ack.ID == "" || ack.State == "" {
		return nil, &TransportError{Code: watch.CodeParse, Message: "fail response missing required fields"}
	}
	return &ack, nil
}

// NeedsSubtool pauses an item in needs_subtool when an upstream feed or DOM
// structure has no matching rule yet.
func (c *Client) NeedsSubtool(ctx context.Context, lease string) (*Ack, error) {
	if strings.TrimSpace(lease) == "" {
		return nil, &ClientError{Message: "lease is required"}
	}
	status, body, err := c.post(ctx, PathNeedsSubtool, needsSubtoolReq{
		Contract: ContractVersion,
		Lease:    lease,
	})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &TransportError{Code: watch.CodeParse, Message: "unexpected HTTP status on needs_subtool"}
	}
	var ack Ack
	if err := decodeStrict(body, &ack); err != nil {
		return nil, err
	}
	if ack.ID == "" || ack.State == "" {
		return nil, &TransportError{Code: watch.CodeParse, Message: "needs_subtool response missing required fields"}
	}
	return &ack, nil
}

func marshalJSONObject(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, &ClientError{Message: "result could not be encoded as JSON"}
	}
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	if trimmed[0] != '{' {
		return nil, &ClientError{Message: "complete result must be a JSON object"}
	}
	return json.RawMessage(trimmed), nil
}

func (c *Client) post(ctx context.Context, path string, payload any) (int, []byte, error) {
	tok, err := c.token()
	if err != nil {
		return 0, nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return 0, nil, &ClientError{Message: "request payload could not be encoded"}
	}
	rawReq := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if len(rawReq) > MaxBodyBytes {
		return 0, nil, &ClientError{Message: "request body exceeds 64 KiB contract limit"}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(rawReq))
	if err != nil {
		return 0, nil, &ClientError{Message: "invalid Muster request"}
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		if watch.IsTimeout(err) || ctx.Err() != nil {
			return 0, nil, &TransportError{Code: watch.CodeTimeout, Message: "request to Muster timed out"}
		}
		return 0, nil, &TransportError{Code: watch.CodeTimeout, Message: "Muster is unreachable"}
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, MaxBodyBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		if watch.IsTimeout(err) {
			return 0, nil, &TransportError{Code: watch.CodeTimeout, Message: "reading Muster response timed out"}
		}
		return 0, nil, &TransportError{Code: watch.CodeParse, Message: "Muster response could not be read"}
	}
	if len(body) > MaxBodyBytes {
		return 0, nil, &TransportError{Code: watch.CodeParse, Message: "Muster response exceeded 64 KiB"}
	}

	if resp.StatusCode >= 400 {
		var ref Refusal
		if derr := decodeStrict(body, &ref); derr != nil {
			if resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout {
				return 0, nil, &TransportError{Code: watch.CodeTimeout, Message: "Muster is restarting or temporarily unavailable"}
			}
			return 0, nil, &TransportError{Code: watch.CodeParse, Message: "Muster returned a non-contract error body"}
		}
		if ref.Error == "" || ref.Cause == "" || ref.Reason == "" {
			return 0, nil, &TransportError{Code: watch.CodeParse, Message: "Muster refusal is missing required fields"}
		}
		switch ref.Error {
		case watch.CodeParse, watch.CodeTimeout, watch.CodeNeedsSubtool:
		default:
			return 0, nil, &TransportError{Code: watch.CodeParse, Message: "Muster refusal carried an unknown error code"}
		}
		ref.Status = resp.StatusCode
		return resp.StatusCode, nil, &RefusalError{Refusal: ref}
	}

	return resp.StatusCode, body, nil
}

func decodeStrict(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return &TransportError{Code: watch.CodeParse, Message: "Muster response did not match contract schema"}
	}
	if _, err := dec.Token(); err != io.EOF {
		return &TransportError{Code: watch.CodeParse, Message: "Muster response had trailing tokens"}
	}
	return nil
}
