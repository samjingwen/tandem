package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/samjingwen/tandem/internal/readmodel"
)

func (c *Client) Manifest(ctx context.Context) (readmodel.Manifest, error) {
	var manifest readmodel.Manifest
	err := c.Do(ctx, http.MethodGet, "/v1/reads/snapshot", "", nil, &manifest)
	if err == nil && (manifest.InstanceID == "" || manifest.Revision.Revision < 0 || manifest.MaxPageBytes != readmodel.MaxPageBytes || manifest.ContentBytes != readmodel.ContentBytes || manifest.MaxRecords != readmodel.MaxRecords) {
		err = errors.New("invalid bounded read manifest")
	}
	return manifest, err
}

func (c *Client) ReadPage(ctx context.Context, revision readmodel.Revision, query url.Values) (readmodel.Page, error) {
	query = cloneQuery(query)
	if query.Get("cursor") == "" {
		query.Set("instanceId", revision.InstanceID)
		query.Set("revision", strconv.FormatInt(revision.Revision, 10))
	}
	var page readmodel.Page
	err := c.Do(ctx, http.MethodGet, "/v1/reads/records?"+query.Encode(), "", nil, &page)
	if err == nil && (page.Revision != revision || len(page.Records) > readmodel.MaxRecords || page.Collection != query.Get("collection")) {
		err = errors.New("invalid or mixed-revision record page")
	}
	return page, err
}

// Walk visits bounded metadata pages without building an inventory in memory.
// It never retries a stale traversal or appends a new revision to partial output.
func (c *Client) Walk(ctx context.Context, revision readmodel.Revision, query url.Values, visit func(readmodel.Record) error) error {
	query = cloneQuery(query)
	for {
		page, err := c.ReadPage(ctx, revision, query)
		if err != nil {
			return err
		}
		for _, record := range page.Records {
			if err := visit(record); err != nil {
				return err
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		if page.NextCursor == query.Get("cursor") || len(page.Records) == 0 {
			return errors.New("non-progressing record cursor")
		}
		query.Set("cursor", page.NextCursor)
	}
}

// WriteRecord streams the complete JSON record, including large accumulated
// instructions/evidence, with constant content-page memory. The caller must
// discard partial output on any error, including stale_read or checksum failure.
func (c *Client) WriteRecord(ctx context.Context, revision readmodel.Revision, record readmodel.Record, out io.Writer) error {
	cursor, offset := record.Content.Cursor, 0
	hash := sha256.New()
	for cursor != "" {
		var page readmodel.ContentPage
		if err := c.Do(ctx, http.MethodGet, "/v1/reads/content?cursor="+url.QueryEscape(cursor), "", nil, &page); err != nil {
			return err
		}
		if page.Revision != revision || page.Key != record.Key || page.Offset != offset || page.TotalBytes != record.Content.Bytes || page.SHA256 != record.Content.SHA256 || len(page.Text) > readmodel.ContentBytes || page.Text == "" || page.NextCursor == cursor {
			return errors.New("invalid, non-progressing, or mixed-revision content page")
		}
		if _, err := io.WriteString(io.MultiWriter(out, hash), page.Text); err != nil {
			return err
		}
		offset += len(page.Text)
		if offset > record.Content.Bytes {
			return errors.New("content exceeds its declared byte length")
		}
		cursor = page.NextCursor
	}
	if offset != record.Content.Bytes || hex.EncodeToString(hash.Sum(nil)) != record.Content.SHA256 {
		return errors.New("incomplete or damaged record content")
	}
	return nil
}

func cloneQuery(query url.Values) url.Values {
	copy := url.Values{}
	for key, values := range query {
		copy[key] = append([]string(nil), values...)
	}
	return copy
}

// ReceiptReadError retains an accepted mutation's identity when its subsequent
// read-only receipt traversal fails. The POST must not be speculatively repeated.
type ReceiptReadError struct {
	OperationID string
	Cause       error
}

func (err *ReceiptReadError) Error() string {
	return "mutation accepted but receipt traversal failed; discard partial output and retain the SAME operation ID and input for reconciliation"
}

func (err *ReceiptReadError) Unwrap() error { return err.Cause }

// WriteMutation negotiates a bounded exact receipt and streams its content when
// necessary. It sends one POST only, retaining the original operation identity,
// and never converts a failed follow-on GET into a rejected mutation.
func (c *Client) WriteMutation(ctx context.Context, path, operationID string, input any, out io.Writer) error {
	copy := *c
	copy.boundedMutations = true
	var data json.RawMessage
	if err := copy.Do(ctx, http.MethodPost, path, operationID, input, &data); err != nil {
		return err
	}
	var receipt readmodel.Receipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return err
	}
	if receipt.Representation == "operation-receipt" {
		if receipt.OperationID != operationID || receipt.Record.Key != operationID {
			return errors.New("mutation receipt identity mismatch")
		}
		if err := c.WriteRecord(ctx, receipt.Revision, receipt.Record, out); err != nil {
			return &ReceiptReadError{OperationID: operationID, Cause: err}
		}
		return nil
	}
	_, err := out.Write(append(data, '\n'))
	return err
}
