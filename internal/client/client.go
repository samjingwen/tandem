// Package client implements a small, non-retrying HTTP client for Tandem v1.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/samjingwen/tandem/core"
)

const (
	DefaultURL       = "http://127.0.0.1:7433"
	maxResponseBytes = 8 << 20
)

type Options struct {
	URL        string
	Token      string
	HTTPClient *http.Client
}

type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// New uses explicit options only. FromEnv is available to opt into environment
// configuration. Redirects are never followed, including for caller HTTP clients.
func New(options Options) (*Client, error) {
	if options.URL == "" {
		options.URL = DefaultURL
	}
	base, err := url.Parse(options.URL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("Tandem URL must be an http(s) server URL without credentials, query, or fragment")
	}
	if base.Path != "" && base.Path != "/" {
		return nil, errors.New("Tandem URL must not include a path")
	}
	base.Path = ""
	if strings.ContainsAny(options.Token, "\r\n") {
		return nil, errors.New("Tandem token must not contain line breaks")
	}
	httpClient := http.Client{Timeout: 35 * time.Second}
	if options.HTTPClient != nil {
		httpClient = *options.HTTPClient
	}
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{base: base, token: options.Token, http: &httpClient}, nil
}

// FromEnv reads only the named public URL and token environment variables, not
// any environment files or authentication caches. The client never logs them.
func FromEnv() (*Client, error) {
	return New(Options{URL: os.Getenv("TANDEM_URL"), Token: os.Getenv("TANDEM_TOKEN")})
}

// Error is a structured API or HTTP protocol failure.
type Error struct {
	StatusCode  int
	Code        string
	Message     string
	OperationID string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// TransportError preserves errors.Is support without printing transport details
// that could contain private configuration. A mutation may have reached the
// server even when no response was received.
type TransportError struct {
	OperationID string
	Uncertain   bool
	Cause       error
}

func (e *TransportError) Error() string {
	if e.Uncertain {
		return "request failed; mutation outcome may be unknown; retry only with the same operation ID and body"
	}
	return "Tandem request failed"
}

func (e *TransportError) Unwrap() error { return e.Cause }

var validOperationID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// ValidateOperationID validates the header syntax, not its durable ownership.
func ValidateOperationID(id string) error {
	if !validOperationID.MatchString(id) {
		return errors.New("operation ID must contain 1-128 ASCII letters, digits, dots, underscores, colons, or hyphens, starting with a letter or digit")
	}
	return nil
}

// Do performs exactly one logical request, never following redirects or retrying
// an HTTP failure. Mutation callers must retain operationID and the exact body.
// Path must be a root-relative /v1/ API path and may include a query string.
func (c *Client) Do(ctx context.Context, method, path, operationID string, input, output any) error {
	if method != http.MethodGet && method != http.MethodPost {
		return errors.New("Tandem client supports only GET and POST")
	}
	mutation := method == http.MethodPost
	if mutation {
		if err := ValidateOperationID(operationID); err != nil {
			return err
		}
	}
	rel, err := url.Parse(path)
	if err != nil || rel.IsAbs() || rel.Host != "" || rel.Fragment != "" || !strings.HasPrefix(rel.Path, "/v1/") {
		return errors.New("API path must be root-relative and start with /v1/")
	}
	for _, segment := range strings.Split(rel.Path, "/") {
		if segment == "." || segment == ".." {
			return errors.New("API path must not contain dot segments")
		}
	}
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return errors.New("request body could not be encoded as JSON")
		}
		if len(data) > 1<<20 {
			return errors.New("request body exceeds 1 MiB")
		}
		body = bytes.NewReader(data)
	} else if mutation {
		// Even bodyless commands use {}. Together with GetBody=nil this also
		// prevents the standard transport's zero-length-request retry path.
		body = strings.NewReader("{}")
	}
	endpoint := c.base.ResolveReference(rel)
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return errors.New("Tandem request could not be constructed")
	}
	// Remove GetBody to prevent a caller's transport from replaying a mutation.
	// The standard transport does not retry a non-idempotent POST.
	if mutation {
		req.GetBody = nil
		req.Header.Set("X-Operation-ID", operationID)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "tandem/"+core.Version)
	if input != nil || mutation {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	response, err := c.http.Do(req)
	if err != nil {
		return &TransportError{OperationID: operationID, Uncertain: mutation, Cause: err}
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return &TransportError{OperationID: operationID, Uncertain: mutation, Cause: err}
	}
	if len(data) > maxResponseBytes {
		return &Error{StatusCode: response.StatusCode, Code: "response_too_large", Message: "response exceeds 8 MiB; mutation outcome may need reconciliation", OperationID: operationID}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var payload struct {
			Error core.Error `json:"error"`
		}
		if json.Unmarshal(data, &payload) != nil || payload.Error.Code == "" || payload.Error.Message == "" {
			payload.Error = core.Error{Code: "http_error", Message: fmt.Sprintf("server returned HTTP %d without a valid API error", response.StatusCode)}
		}
		return &Error{StatusCode: response.StatusCode, Code: payload.Error.Code, Message: payload.Error.Message, OperationID: operationID}
	}
	if !json.Valid(data) {
		return &Error{StatusCode: response.StatusCode, Code: "invalid_response", Message: "server response is not valid JSON; mutation outcome may need reconciliation", OperationID: operationID}
	}
	if output != nil && json.Unmarshal(data, output) != nil {
		return &Error{StatusCode: response.StatusCode, Code: "invalid_response", Message: "server response does not match the expected JSON shape", OperationID: operationID}
	}
	return nil
}
