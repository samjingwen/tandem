package client_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/samjingwen/tandem/internal/client"
	"github.com/samjingwen/tandem/internal/readmodel"
)

func TestFailedReceiptReadRetainsMutationIdentityWithoutRetry(t *testing.T) {
	posts, gets := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
			if r.Header.Get("Prefer") != "return=bounded" || r.Header.Get("X-Operation-ID") != "retained-id" {
				t.Error("missing exact mutation identity/negotiation")
			}
			_ = json.NewEncoder(w).Encode(readmodel.Receipt{Representation: "operation-receipt", OperationID: "retained-id", Revision: readmodel.Revision{InstanceID: "fixture", Revision: 1}, Record: readmodel.Record{Key: "retained-id", Content: readmodel.ContentRef{Cursor: "issued-cursor", Bytes: 100}}})
			return
		}
		gets++
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"code":"unavailable","message":"fixture receipt read unavailable"}}`)
	}))
	defer server.Close()
	c, err := client.New(client.Options{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	err = c.WriteMutation(t.Context(), "/v1/tasks", "retained-id", map[string]string{"title": "fixture"}, io.Discard)
	var receipt *client.ReceiptReadError
	var cause *client.Error
	if !errors.As(err, &receipt) || receipt.OperationID != "retained-id" || !errors.As(err, &cause) || cause.Code != "unavailable" {
		t.Fatal("receipt failure erased the accepted mutation identity or cause", err)
	}
	if posts != 1 || gets != 1 {
		t.Fatalf("receipt failure retried mutation or read: posts=%d gets=%d", posts, gets)
	}
}
