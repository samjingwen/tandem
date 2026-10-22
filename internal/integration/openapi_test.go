package integration_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/readmodel"
)

// A typed OpenAPI consumer, plus a validator for the JSON Schema keywords used
// by this contract. This validates real payloads, not strings in a config file.
// It is intentionally a test consumer, not a general-purpose schema library.
type openAPI struct {
	OpenAPI string `json:"openapi"`
	Info    struct {
		Version string `json:"version"`
	} `json:"info"`
	Paths map[string]struct {
		Parameters []parameter `json:"parameters"`
		Get        *operation  `json:"get"`
		Post       *operation  `json:"post"`
	} `json:"paths"`
	Components struct {
		Schemas    map[string]schema    `json:"schemas"`
		Parameters map[string]parameter `json:"parameters"`
	} `json:"components"`
}

type parameter struct {
	Ref      string `json:"$ref"`
	Name     string `json:"name"`
	In       string `json:"in"`
	Required bool   `json:"required"`
	Schema   schema `json:"schema"`
}

type operation struct {
	ID          string      `json:"operationId"`
	Parameters  []parameter `json:"parameters"`
	RequestBody struct {
		Required bool                 `json:"required"`
		Content  map[string]mediaType `json:"content"`
	} `json:"requestBody"`
	Responses map[string]struct {
		Content map[string]mediaType `json:"content"`
	} `json:"responses"`
}

type mediaType struct {
	Schema schema `json:"schema"`
}

type schema struct {
	Ref                  string            `json:"$ref"`
	Type                 string            `json:"type"`
	Format               string            `json:"format"`
	Pattern              string            `json:"pattern"`
	Required             []string          `json:"required"`
	Properties           map[string]schema `json:"properties"`
	AdditionalProperties json.RawMessage   `json:"additionalProperties"`
	Items                *schema           `json:"items"`
	OneOf                []schema          `json:"oneOf"`
	Enum                 []any             `json:"enum"`
	Minimum              *float64          `json:"minimum"`
	MinLength            *int              `json:"minLength"`
	MaxLength            *int              `json:"maxLength"`
	MinItems             *int              `json:"minItems"`
	MaxItems             *int              `json:"maxItems"`
}

func (doc openAPI) validate(s schema, value any) error {
	if s.Ref != "" {
		name, ok := strings.CutPrefix(s.Ref, "#/components/schemas/")
		if !ok {
			return fmt.Errorf("unsupported schema reference %s", s.Ref)
		}
		resolved, exists := doc.Components.Schemas[name]
		if !exists {
			return fmt.Errorf("unresolved schema reference %s", s.Ref)
		}
		return doc.validate(resolved, value)
	}
	if len(s.OneOf) != 0 {
		matches := 0
		for _, candidate := range s.OneOf {
			if doc.validate(candidate, value) == nil {
				matches++
			}
		}
		if matches != 1 {
			return fmt.Errorf("oneOf matched %d schemas", matches)
		}
	}
	if len(s.Enum) != 0 {
		matches := false
		for _, candidate := range s.Enum {
			matches = matches || reflect.DeepEqual(candidate, value)
		}
		if !matches {
			return fmt.Errorf("value not in enum: %v", value)
		}
	}
	switch s.Type {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return errors.New("expected object")
		}
		for _, name := range s.Required {
			if _, exists := object[name]; !exists {
				return fmt.Errorf("required field %s absent", name)
			}
		}
		for name, child := range object {
			property, exists := s.Properties[name]
			if !exists {
				switch string(s.AdditionalProperties) {
				case "false":
					return fmt.Errorf("unknown field %s", name)
				case "", "true":
					continue
				default:
					if err := json.Unmarshal(s.AdditionalProperties, &property); err != nil {
						return err
					}
				}
			}
			if err := doc.validate(property, child); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	case "array":
		array, ok := value.([]any)
		if !ok || (s.MinItems != nil && len(array) < *s.MinItems) || (s.MaxItems != nil && len(array) > *s.MaxItems) {
			return errors.New("expected array with required item count")
		}
		if s.Items != nil {
			for i, child := range array {
				if err := doc.validate(*s.Items, child); err != nil {
					return fmt.Errorf("item %d: %w", i, err)
				}
			}
		}
	case "string":
		text, ok := value.(string)
		if !ok {
			return errors.New("expected string")
		}
		length := utf8.RuneCountInString(text)
		if (s.MinLength != nil && length < *s.MinLength) || (s.MaxLength != nil && length > *s.MaxLength) {
			return errors.New("string length outside bounds")
		}
		if s.Pattern != "" {
			matched, err := regexp.MatchString(s.Pattern, text)
			if err != nil || !matched {
				return errors.New("string does not match pattern")
			}
		}
		if s.Format == "date-time" {
			if _, err := time.Parse(time.RFC3339Nano, text); err != nil {
				return errors.New("invalid date-time")
			}
		}
	case "integer":
		number, ok := value.(float64)
		if !ok || math.Trunc(number) != number || (s.Minimum != nil && number < *s.Minimum) {
			return errors.New("expected integer within bounds")
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return errors.New("expected boolean")
		}
	case "": // An unconstrained arbitrary JSON value, such as a decision answer.
	default:
		return fmt.Errorf("unsupported schema type %s", s.Type)
	}
	return nil
}

func validateReferences(root any, value any) error {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "$ref" {
				ref, ok := child.(string)
				if !ok || !strings.HasPrefix(ref, "#/") {
					return errors.New("contract references must be local JSON pointers")
				}
				var target = root
				for _, segment := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
					object, ok := target.(map[string]any)
					if !ok {
						return fmt.Errorf("invalid reference %s", ref)
					}
					target, ok = object[segment]
					if !ok {
						return fmt.Errorf("missing reference %s", ref)
					}
				}
			}
			if err := validateReferences(root, child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := validateReferences(root, child); err != nil {
				return err
			}
		}
	}
	return nil
}

func TestOpenAPIConsumerValidatesEngineWire(t *testing.T) {
	f := setup(t)
	var raw json.RawMessage
	f.do(t, 0, http.MethodGet, "/v1/openapi.json", "", nil, &raw)
	var doc openAPI
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	if err := validateReferences(document, document); err != nil {
		t.Fatal(err)
	}
	if doc.OpenAPI != "3.1.0" || doc.Info.Version != core.Version {
		t.Fatalf("unexpected contract version: %+v", doc.Info)
	}
	ids := map[string]bool{}
	for path, item := range doc.Paths {
		for _, op := range []*operation{item.Get, item.Post} {
			if op == nil {
				continue
			}
			if op.ID == "" || ids[op.ID] {
				t.Fatalf("invalid/duplicate operation ID on %s", path)
			}
			ids[op.ID] = true
			if op == item.Post {
				found := false
				for _, parameter := range op.Parameters {
					if parameter.Ref != "" {
						parameter = doc.Components.Parameters[strings.TrimPrefix(parameter.Ref, "#/components/parameters/")]
					}
					found = found || (parameter.Name == "X-Operation-ID" && parameter.In == "header" && parameter.Required)
				}
				if !found || op.RequestBody.Content["application/json"].Schema.Ref == "" {
					t.Fatalf("mutation contract missing operation ID/body schema: %s", path)
				}
			}
		}
	}
	task, attempt := f.task(t)
	f.result(t, task, attempt, task.Generation, "schema-result")
	f.runtime.SetDecisions(attempt.SessionRef, core.Decision{RuntimeRef: "request/one", Kind: "question", Prompt: "Continue?"})
	f.tick(t)
	for path, concrete := range map[string]string{
		"/v1/health": "/v1/health", "/v1/ready": "/v1/ready", "/v1/version": "/v1/version", "/v1/runtime": "/v1/runtime", "/v1/doctor": "/v1/doctor",
		"/v1/projects": "/v1/projects", "/v1/tasks": "/v1/tasks", "/v1/decisions": "/v1/decisions", "/v1/snapshot": "/v1/snapshot",
		"/v1/tasks/{taskId}": "/v1/tasks/" + task.ID, "/v1/tasks/{taskId}/results": "/v1/tasks/" + task.ID + "/results", "/v1/openapi.json": "/v1/openapi.json",
		"/v1/reads/snapshot": "/v1/reads/snapshot",
	} {
		var payload any
		f.do(t, 1, http.MethodGet, concrete, "", nil, &payload)
		s := doc.Paths[path].Get.Responses["200"].Content["application/json"].Schema
		if err := doc.validate(s, payload); err != nil {
			t.Fatalf("real HTTP response violates %s: %v", path, err)
		}
	}
	manifest, err := f.clients[0].Manifest(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"projects", "tasks", "attempts", "commands", "decisions", "results"} {
		page, err := f.clients[0].ReadPage(f.ctx, manifest.Revision, url.Values{"collection": {kind}})
		if err != nil {
			t.Fatal(err)
		}
		checkWireSchema(t, doc, "ReadPage", page)
		for _, row := range page.Records {
			var content readmodel.ContentPage
			f.do(t, 0, http.MethodGet, "/v1/reads/content?cursor="+url.QueryEscape(row.Content.Cursor), "", nil, &content)
			checkWireSchema(t, doc, "ContentPage", content)
		}
	}
	resultSchema := doc.Paths["/v1/tasks/{taskId}/results"].Post.RequestBody.Content["application/json"].Schema
	for _, generation := range []any{nil, float64(0), float64(-1), float64(1.5), "1"} {
		body := map[string]any{"attemptId": attempt.ID, "summary": "wrong generation", "evidence": []any{map[string]any{"name": "test", "reference": "artifact://test", "passed": true}}}
		if generation != nil {
			body["generation"] = generation
		}
		if err := doc.validate(resultSchema, body); err == nil {
			t.Fatalf("schema accepted invalid generation %v", generation)
		}
		if err := f.clients[0].Do(f.ctx, http.MethodPost, "/v1/tasks/"+task.ID+"/results", core.ID("schema-invalid_"), body, nil); err == nil {
			t.Fatalf("API accepted invalid generation %v", generation)
		}
	}
	valid := map[string]any{"attemptId": attempt.ID, "generation": float64(2), "summary": "valid", "evidence": []any{map[string]any{"name": "test", "reference": "artifact://test", "passed": true}}}
	if err := doc.validate(resultSchema, valid); err != nil {
		t.Fatal("schema rejected valid result", err)
	}
	valid["unknown"] = true
	if err := doc.validate(resultSchema, valid); err == nil {
		t.Fatal("schema accepted unknown request field")
	}
}

func checkWireSchema(t *testing.T, doc openAPI, name string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var wire any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if err := doc.validate(doc.Components.Schemas[name], wire); err != nil {
		t.Fatalf("%s violates its wire schema: %v", name, err)
	}
}

func TestNegotiatedReceiptMatchesSchemaAndLegacyReplay(t *testing.T) {
	f := setup(t)
	var task core.Task
	f.do(t, 0, http.MethodPost, "/v1/tasks", "schema-large-create", core.CreateTask{ProjectID: f.project.ID, Title: "receipt fixture", Instructions: strings.Repeat("x", 128<<10)}, &task)
	var legacy core.Task
	f.do(t, 0, http.MethodPost, "/v1/tasks/"+task.ID+"/followup", "schema-large-followup", core.Followup{Text: strings.Repeat("y", 128<<10)}, &legacy)
	if len(legacy.Instructions) <= 256<<10 {
		t.Fatal("fixture failed to cross the negotiated representation boundary")
	}
	data, err := json.Marshal(core.Followup{Text: strings.Repeat("y", 128<<10)})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(f.ctx, http.MethodPost, f.server.URL+"/v1/tasks/"+task.ID+"/followup", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer integration-test-token")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Operation-ID", "schema-large-followup")
	request.Header.Set("Prefer", "return=bounded")
	response, err := f.runners[1].HTTPClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var receipt readmodel.Receipt
	if err := json.NewDecoder(response.Body).Decode(&receipt); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || response.Header.Get("Preference-Applied") != "return=bounded" || receipt.OperationID != "schema-large-followup" || receipt.Representation != "operation-receipt" {
		t.Fatal("missing explicit bounded receipt negotiation", response.StatusCode)
	}
	var doc openAPI
	f.do(t, 0, http.MethodGet, "/v1/openapi.json", "", nil, &doc)
	checkWireSchema(t, doc, "ReadReceipt", receipt)
	var content bytes.Buffer
	if err := f.clients[0].WriteRecord(f.ctx, receipt.Revision, receipt.Record, &content); err != nil {
		t.Fatal(err)
	}
	var decoded core.Task
	if err := json.Unmarshal(content.Bytes(), &decoded); err != nil || !reflect.DeepEqual(decoded, legacy) {
		t.Fatal("negotiated receipt differs from the exact legacy replay", err)
	}
	var ignored any
	err = f.clients[0].Do(f.ctx, http.MethodGet, "/v1/reads/records?collection=receipts&instanceId="+url.QueryEscape(receipt.InstanceID)+fmt.Sprintf("&revision=%d", receipt.Revision.Revision), "", nil, &ignored)
	if err == nil {
		t.Fatal("receipt read surface enumerated the private journal")
	}
}
