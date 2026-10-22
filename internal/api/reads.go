package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/readmodel"
)

// Cursors are authenticated scope claims, not retained snapshots. Every request
// reads one atomic store revision and refuses to mix it with an older traversal.
// A new handler/process rotates the key and requires a new manifest/traversal.
type readCursor struct {
	readmodel.Revision
	Type       string `json:"type"`
	Collection string `json:"collection"`
	ProjectID  string `json:"projectId,omitempty"`
	TaskID     string `json:"taskId,omitempty"`
	State      string `json:"state,omitempty"`
	Status     string `json:"status,omitempty"`
	SessionRef string `json:"sessionRef,omitempty"`
	Key        string `json:"key,omitempty"`
	Limit      int    `json:"limit"`
	Offset     int    `json:"offset"`
}

func (h *handler) signCursor(cursor readCursor) string {
	data, _ := json.Marshal(cursor)
	mac := hmac.New(sha256.New, h.cursorKey[:])
	_, _ = mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(data) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (h *handler) parseCursor(value string) (readCursor, bool) {
	var cursor readCursor
	if len(value) > 8192 {
		return cursor, false
	}
	body, signature, ok := strings.Cut(value, ".")
	data, err := base64.RawURLEncoding.DecodeString(body)
	if !ok || err != nil {
		return cursor, false
	}
	got, err := base64.RawURLEncoding.DecodeString(signature)
	mac := hmac.New(sha256.New, h.cursorKey[:])
	_, _ = mac.Write(data)
	if err != nil || !hmac.Equal(got, mac.Sum(nil)) || json.Unmarshal(data, &cursor) != nil {
		return cursor, false
	}
	return cursor, cursor.Offset >= 0
}

func (h *handler) boundedRead(w http.ResponseWriter, r *http.Request, path string) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || !readQueryValid(query, path) {
		writeError(w, 400, "invalid_request", "unsupported, repeated, or malformed read parameters")
		return
	}
	if path == "content" && h.cachedContent(w, r, query) {
		return
	}
	state, err := h.service.Snapshot(r.Context())
	if err != nil {
		serviceError(w, err)
		return
	}
	revision := readmodel.Revision{InstanceID: state.InstanceID, Revision: state.Revision}
	if path == "snapshot" {
		writeBoundedJSON(w, readmodel.Manifest{Revision: revision, Collections: map[string]int{
			"projects": len(state.Projects), "tasks": len(state.Tasks), "attempts": len(state.Attempts),
			"commands": len(state.Commands), "decisions": len(state.Decisions), "results": len(state.Results),
		}, MaxPageBytes: readmodel.MaxPageBytes, ContentBytes: readmodel.ContentBytes, MaxRecords: readmodel.MaxRecords})
		return
	}
	cursor, ok := h.readScope(query, path)
	if !ok {
		code := "invalid_request"
		if query.Has("cursor") {
			code = "invalid_cursor"
		}
		writeError(w, 400, code, "invalid read parameters, cursor, or changed traversal scope; start from /v1/reads/snapshot")
		return
	}
	if cursor.Revision.Revision != revision.Revision || cursor.InstanceID != revision.InstanceID {
		writeError(w, 409, "stale_read", "instance or revision changed; discard partial traversal and start from /v1/reads/snapshot")
		return
	}
	rows, ok := readRows(state, cursor)
	if !ok {
		writeError(w, 400, "invalid_request", "unknown collection or unsupported collection filters")
		return
	}
	if path == "content" {
		if len(rows) != 1 {
			writeError(w, 404, "not_found", "content record is unavailable")
			return
		}
		h.readContent(w, cursor, rows[0])
		return
	}
	if cursor.Key != "" && len(rows) == 0 {
		writeError(w, 404, "not_found", "record is unavailable")
		return
	}
	h.readRecords(w, cursor, rows)
}

func readQueryValid(query url.Values, path string) bool {
	for key, values := range query {
		if len(values) != 1 || path == "snapshot" || len(values[0]) > 8192 {
			return false
		}
		switch key {
		case "cursor", "collection", "instanceId", "revision", "projectId", "taskId", "state", "status", "sessionRef", "key", "limit":
		default:
			return false
		}
	}
	return true
}

func (h *handler) readScope(query url.Values, kind string) (readCursor, bool) {
	if token := query.Get("cursor"); token != "" {
		cursor, ok := h.parseCursor(token)
		if !ok || cursor.Type != kind {
			return cursor, false
		}
		// Callers can repeat scope as a guard, but can never widen or change it.
		values := cursor.values()
		for key := range query {
			if key != "cursor" && query.Get(key) != values.Get(key) {
				return cursor, false
			}
		}
		return cursor, true
	}
	cursor := readCursor{Type: kind, Collection: query.Get("collection"), ProjectID: query.Get("projectId"), TaskID: query.Get("taskId"), State: query.Get("state"), Status: query.Get("status"), SessionRef: query.Get("sessionRef"), Key: query.Get("key"), Limit: 32}
	cursor.InstanceID = query.Get("instanceId")
	var err error
	cursor.Revision.Revision, err = strconv.ParseInt(query.Get("revision"), 10, 64)
	if err != nil || cursor.Revision.Revision < 0 || cursor.InstanceID == "" || kind != "records" {
		return cursor, false
	}
	if query.Has("limit") {
		cursor.Limit, err = strconv.Atoi(query.Get("limit"))
		if err != nil || cursor.Limit < 1 || cursor.Limit > readmodel.MaxRecords {
			return cursor, false
		}
	}
	return cursor, true
}

func (cursor readCursor) values() url.Values {
	return url.Values{"instanceId": {cursor.InstanceID}, "revision": {strconv.FormatInt(cursor.Revision.Revision, 10)}, "collection": {cursor.Collection}, "projectId": {cursor.ProjectID}, "taskId": {cursor.TaskID}, "state": {cursor.State}, "status": {cursor.Status}, "sessionRef": {cursor.SessionRef}, "key": {cursor.Key}, "limit": {strconv.Itoa(cursor.Limit)}}
}

type readRow struct {
	key   string
	value any
	order int64
}

func readRows(state core.State, cursor readCursor) ([]readRow, bool) {
	rows := []readRow{}
	add := func(key, projectID, taskID, status, sessionRef string, value any, order int64) {
		if (cursor.Key != "" && cursor.Key != key) || (cursor.ProjectID != "" && cursor.ProjectID != projectID) || (cursor.TaskID != "" && cursor.TaskID != taskID) || (cursor.State != "" && cursor.State != status) || (cursor.Status != "" && cursor.Status != status) || (cursor.SessionRef != "" && cursor.SessionRef != sessionRef) {
			return
		}
		rows = append(rows, readRow{key: key, value: value, order: order})
	}
	project := func(taskID string) string { return state.Tasks[taskID].ProjectID }
	if cursor.State != "" && cursor.Collection != "tasks" || cursor.Status != "" && cursor.Collection != "decisions" || cursor.SessionRef != "" && cursor.Collection != "attempts" || cursor.TaskID != "" && cursor.Collection == "projects" {
		return nil, false
	}
	switch cursor.Collection {
	case "projects":
		for key, value := range state.Projects {
			add(key, value.ID, "", "", "", value, 0)
		}
	case "tasks":
		for key, value := range state.Tasks {
			add(key, value.ProjectID, value.ID, value.State, "", value, 0)
		}
	case "attempts":
		for key, value := range state.Attempts {
			add(key, project(value.TaskID), value.TaskID, "", value.SessionRef, value, 0)
		}
	case "commands":
		for key, value := range state.Commands {
			add(key, project(value.TaskID), value.TaskID, "", "", value, value.Sequence)
		}
	case "decisions":
		for key, value := range state.Decisions {
			add(key, project(value.TaskID), value.TaskID, value.Status, "", value, 0)
		}
	case "results":
		for key, value := range state.Results {
			add(key, project(value.TaskID), value.TaskID, "", "", value, int64(value.Generation))
		}
	case "receipts":
		// Receipts are addressable only by a caller-retained exact operation ID.
		// Never enumerate the private journal or expose its fingerprint metadata.
		if cursor.Key == "" || cursor.ProjectID != "" || cursor.TaskID != "" {
			return nil, false
		}
		if value, exists := state.Operations[cursor.Key]; exists {
			add(cursor.Key, "", "", "", "", value.Outcome, 0)
		}
	default:
		return nil, false
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].order != rows[j].order {
			return rows[i].order < rows[j].order
		}
		return rows[i].key < rows[j].key
	})
	return rows, true
}

func (h *handler) readRecords(w http.ResponseWriter, cursor readCursor, rows []readRow) {
	page := readmodel.Page{Revision: cursor.Revision, Collection: cursor.Collection, Records: []readmodel.Record{}}
	if cursor.Offset > len(rows) {
		writeError(w, 400, "invalid_cursor", "record offset is outside the traversal")
		return
	}
	for index := cursor.Offset; index < len(rows) && len(page.Records) < cursor.Limit; index++ {
		row := rows[index]
		data, err := json.Marshal(row.value)
		if err != nil {
			serviceError(w, err)
			return
		}
		content := cursor
		content.Type, content.Key, content.Offset = "content", row.key, 0
		h.readCache.put(content, data)
		hash := sha256.Sum256(data)
		record := readmodel.Record{Key: row.key, Summary: readSummary(data), Content: readmodel.ContentRef{Cursor: h.signCursor(content), Bytes: len(data), SHA256: hex.EncodeToString(hash[:])}}
		page.Records = append(page.Records, record)
		next := cursor
		next.Offset = index + 1
		page.NextCursor = ""
		if next.Offset < len(rows) {
			page.NextCursor = h.signCursor(next)
		}
		encoded, err := json.Marshal(page)
		if err != nil {
			serviceError(w, err)
			return
		}
		if len(encoded)+1 > readmodel.MaxPageBytes {
			page.Records = page.Records[:len(page.Records)-1]
			next.Offset = index
			page.NextCursor = h.signCursor(next)
			break
		}
	}
	if len(page.Records) == 0 && cursor.Offset < len(rows) {
		writeError(w, 500, "internal", "record navigation metadata exceeds the page budget")
		return
	}
	writeBoundedJSON(w, page)
}

// Summary contains only small scalar navigation/status fields. It never claims
// to be a full core record. Omitted fields remain in the exact Content record.
func readSummary(data []byte) map[string]any {
	var full map[string]any
	_ = json.Unmarshal(data, &full)
	summary := map[string]any{}
	for _, key := range strings.Fields("id projectId taskId parentId attemptId generation role state desired status kind sequence accepted late phase observed sessionRef title name path baseRef") {
		value, exists := full[key]
		if !exists {
			continue
		}
		if text, ok := value.(string); ok && len(text) > 1024 {
			continue
		}
		summary[key] = value
	}
	if workspace, ok := full["workspace"].(map[string]any); ok {
		if path, ok := workspace["path"].(string); ok && len(path) <= 4096 {
			summary["workspacePath"] = path
		}
	}
	return summary
}

func (h *handler) readContent(w http.ResponseWriter, cursor readCursor, row readRow) {
	data, err := json.Marshal(row.value)
	if err != nil {
		serviceError(w, err)
		return
	}
	h.readCache.put(cursor, data)
	hash := sha256.Sum256(data)
	h.readContentBytes(w, cursor, data, hex.EncodeToString(hash[:]))
}

func (h *handler) readContentBytes(w http.ResponseWriter, cursor readCursor, data []byte, checksum string) {
	if cursor.Offset > len(data) || (cursor.Offset < len(data) && !utf8.RuneStart(data[cursor.Offset])) {
		writeError(w, 400, "invalid_cursor", "content offset is invalid")
		return
	}
	end := min(len(data), cursor.Offset+readmodel.ContentBytes)
	for end < len(data) && !utf8.RuneStart(data[end]) {
		end--
	}
	page := readmodel.ContentPage{Revision: cursor.Revision, Collection: cursor.Collection, Key: cursor.Key, Offset: cursor.Offset, TotalBytes: len(data), Text: string(data[cursor.Offset:end]), SHA256: checksum}
	if end < len(data) {
		cursor.Offset = end
		page.NextCursor = h.signCursor(cursor)
	}
	writeBoundedJSON(w, page)
}

func (h *handler) cachedContent(w http.ResponseWriter, r *http.Request, query url.Values) bool {
	cursor, ok := h.readScope(query, "content")
	if !ok {
		return false
	}
	data, exists := h.readCache.get(cursor)
	reader, supportsRevision := h.service.(core.RevisionSource)
	if !exists || !supportsRevision {
		return false
	}
	instance, revision, err := reader.ReadRevision(r.Context())
	if err != nil {
		serviceError(w, err)
		return true
	}
	if instance != cursor.InstanceID || revision != cursor.Revision.Revision {
		writeError(w, 409, "stale_read", "instance or revision changed; discard partial traversal and start from /v1/reads/snapshot")
		return true
	}
	h.readContentBytes(w, cursor, data.data, data.sha256)
	return true
}

func writeBoundedJSON(w http.ResponseWriter, value any) {
	data, err := json.Marshal(value)
	if err != nil || len(data)+1 > readmodel.MaxPageBytes {
		writeError(w, 500, "internal", "bounded read could not be encoded")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(data, '\n'))
}

func (h *handler) mutationResult(w http.ResponseWriter, r *http.Request, value any, err error) {
	if err != nil {
		serviceError(w, err)
		return
	}
	data, err := json.Marshal(value)
	if err != nil {
		serviceError(w, err)
		return
	}
	if r.Header.Get("Prefer") != "return=bounded" || len(data)+1 <= readmodel.MaxPageBytes {
		writeJSON(w, http.StatusOK, value)
		return
	}
	state, err := h.service.Snapshot(r.Context())
	if err != nil {
		serviceError(w, err)
		return
	}
	op := r.Header.Get("X-Operation-ID")
	outcome, exists := state.Operations[op]
	if !exists {
		writeError(w, 500, "internal", "durable mutation receipt unavailable; retain the operation ID for reconciliation")
		return
	}
	data, err = json.Marshal(outcome.Outcome)
	if err != nil {
		serviceError(w, err)
		return
	}
	revision := readmodel.Revision{InstanceID: state.InstanceID, Revision: state.Revision}
	cursor := readCursor{Revision: revision, Type: "content", Collection: "receipts", Key: op, Limit: 1}
	h.readCache.put(cursor, data)
	hash := sha256.Sum256(data)
	w.Header().Set("Preference-Applied", "return=bounded")
	writeBoundedJSON(w, readmodel.Receipt{Revision: revision, Representation: "operation-receipt", OperationID: op, Record: readmodel.Record{Key: op, Summary: readSummary(data), Content: readmodel.ContentRef{Cursor: h.signCursor(cursor), Bytes: len(data), SHA256: hex.EncodeToString(hash[:])}}})
}
