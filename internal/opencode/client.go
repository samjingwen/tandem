// Package opencode implements the OpenCode 2.1.0 HTTP runtime contract.
package opencode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/samjingwen/tandem/core"
)

const Version = "2.1.0"

// SourceRevision is the upstream source used to verify this adapter's wire API.
const SourceRevision = "60c68d145a14127ce633e26aa5fdfb882a7baa6d"

var (
	ErrVersion   = errors.New("incompatible OpenCode version")
	ErrOwnership = errors.New("OpenCode ownership or admission collision")
)

type Model struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Variant    string `json:"variant,omitempty"`
}

type Options struct {
	URL string
	// Password defaults to the explicitly supplied OPENCODE_PASSWORD environment variable.
	Password        string
	ExpectedVersion string
	Model           *Model
	Agent           string
}

type Runtime struct {
	base     string
	password string
	expected string
	id       string
	model    *Model
	agent    string
	client   *http.Client
}

var _ core.Runtime = (*Runtime)(nil)

func New(options Options) (*Runtime, error) {
	u, err := url.Parse(options.URL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, core.Invalid("OpenCode URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	base := u.String()
	if options.ExpectedVersion == "" {
		options.ExpectedVersion = Version
	}
	if options.Password == "" {
		options.Password = os.Getenv("OPENCODE_PASSWORD")
	}
	if options.Model != nil && (options.Model.ID == "" || options.Model.ProviderID == "") {
		return nil, core.Invalid("OpenCode model requires id and providerID")
	}
	var model *Model
	if options.Model != nil {
		copied := *options.Model
		model = &copied
	}
	return &Runtime{
		base:     base,
		password: options.Password,
		expected: options.ExpectedVersion,
		id:       "opencode_" + digest(base),
		model:    model,
		agent:    options.Agent,
		client:   &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

func digest(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:16])
}

func (*Runtime) Reference(attemptID string) string {
	return "ses_" + digest("tandem/session/"+attemptID)
}

func (r *Runtime) Info(ctx context.Context) (core.RuntimeInfo, error) {
	var response struct {
		Version string `json:"version"`
	}
	err := r.request(ctx, http.MethodGet, "/api/info", nil, &response)
	info := core.RuntimeInfo{ID: r.id, Version: response.Version, Compatible: err == nil && response.Version == r.expected && response.Version == Version}
	if err != nil {
		return info, err
	}
	if !info.Compatible {
		info.Detail = "requires OpenCode " + Version + " and the configured exact expected version"
	}
	return info, nil
}

func (r *Runtime) compatible(ctx context.Context) error {
	info, err := r.Info(ctx)
	if err != nil {
		return err
	}
	if !info.Compatible {
		return ErrVersion
	}
	return nil
}

type httpError struct{ status int }

func (e *httpError) Error() string { return fmt.Sprintf("OpenCode HTTP status %d", e.status) }
func statusIs(err error, code int) bool {
	var e *httpError
	return errors.As(err, &e) && e.status == code
}

// request never retries. Transport, server, and malformed-success failures on writes
// preserve uncertainty; response bodies and credentials are deliberately not logged.
func (r *Runtime) request(ctx context.Context, method, path string, input, output any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.base+path, body)
	if err != nil {
		return err
	}
	r.authenticate(req)
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	response, err := r.client.Do(req)
	write := method != http.MethodGet
	if err != nil {
		if write {
			return core.ErrUncertain
		}
		return fmt.Errorf("OpenCode request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if write && (response.StatusCode >= 500 || response.StatusCode < 400 || response.StatusCode == http.StatusRequestTimeout) {
			return core.ErrUncertain
		}
		return &httpError{status: response.StatusCode}
	}
	if output == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err == nil && len(data) > 8<<20 {
		err = errors.New("response exceeds size limit")
	}
	if err == nil {
		err = json.Unmarshal(data, output)
	}
	if err != nil {
		if write {
			return core.ErrUncertain
		}
		return errors.New("invalid OpenCode JSON response")
	}
	return nil
}

func (r *Runtime) authenticate(req *http.Request) { req.SetBasicAuth("opencode", r.password) }

type envelope[T any] struct {
	Data *T `json:"data"`
}

func getData[T any](r *Runtime, ctx context.Context, path string) (T, error) {
	var result envelope[T]
	err := r.request(ctx, http.MethodGet, path, nil, &result)
	if err == nil && result.Data == nil {
		err = errors.New("missing OpenCode data envelope")
	}
	if err != nil {
		var zero T
		return zero, err
	}
	return *result.Data, nil
}

func sessionPath(spec core.SessionSpec) string {
	return "/api/session/" + url.PathEscape(spec.Reference)
}
