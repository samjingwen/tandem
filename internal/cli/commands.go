package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/samjingwen/tandem/core"
)

func (runner Runner) command(args []string) (request, error) {
	if len(args) == 0 {
		return request{}, errors.New("a command is required; use --help")
	}
	if args[0] == "snapshot" || args[0] == "doctor" || args[0] == "version" {
		if len(args) != 1 {
			return request{}, fmt.Errorf("%s does not accept arguments", args[0])
		}
		return request{method: http.MethodGet, path: "/v1/" + args[0]}, nil
	}
	if len(args) < 2 {
		return request{}, errors.New("a subcommand is required; use --help")
	}
	switch args[0] {
	case "projects":
		return runner.projects(args[1], args[2:])
	case "tasks":
		return runner.tasks(args[1], args[2:])
	case "decisions":
		return runner.decisions(args[1], args[2:])
	default:
		return request{}, errors.New("unknown command; use --help")
	}
}

// parse accepts command-specific long options on either side of positional IDs.
// Unknown and duplicate options are rejected rather than silently ignored.
func parse(args []string, values, booleans string) (map[string]string, []string, error) {
	allowed := map[string]bool{}
	for _, name := range strings.Fields(values) {
		allowed[name] = false
	}
	for _, name := range strings.Fields(booleans) {
		allowed[name] = true
	}
	options := map[string]string{}
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		boolean, known := allowed[name]
		if !strings.HasPrefix(arg, "--") || !known {
			return nil, nil, errors.New("unsupported option; use --help")
		}
		if _, exists := options[name]; exists {
			return nil, nil, fmt.Errorf("--%s may only be supplied once", name)
		}
		if boolean {
			if !hasValue {
				value = "true"
			}
			if value != "true" && value != "false" {
				return nil, nil, fmt.Errorf("--%s expects true or false", name)
			}
		} else if !hasValue {
			i++
			if i >= len(args) {
				return nil, nil, fmt.Errorf("--%s requires a value", name)
			}
			value = args[i]
		}
		options[name] = value
	}
	return options, positional, nil
}

func required(options map[string]string, names ...string) error {
	for _, name := range names {
		if strings.TrimSpace(options[name]) == "" {
			return fmt.Errorf("--%s is required", name)
		}
	}
	return nil
}

func positionals(args []string, count int) error {
	if len(args) != count {
		return fmt.Errorf("expected %d positional ID(s); use --help", count)
	}
	for _, id := range args {
		if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\\r\n") {
			return errors.New("IDs must be nonempty single path segments")
		}
	}
	return nil
}

func (runner Runner) text(value string) (string, error) {
	if value != "-" {
		return value, nil
	}
	if runner.In == nil {
		return "", errors.New("standard input is unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(runner.In, (1<<20)+1))
	if err != nil {
		return "", errors.New("standard input could not be read")
	}
	if len(data) > 1<<20 {
		return "", errors.New("standard input exceeds 1 MiB")
	}
	return string(data), nil
}

func (runner Runner) projects(command string, args []string) (request, error) {
	if command == "list" {
		if len(args) != 0 {
			return request{}, errors.New("projects list does not accept arguments")
		}
		return request{method: http.MethodGet, path: "/v1/projects", table: "projects"}, nil
	}
	if command != "register" {
		return request{}, errors.New("unknown projects command; use --help")
	}
	opts, ids, err := parse(args, "name path base-ref instructions", "")
	if err != nil {
		return request{}, err
	}
	if err := positionals(ids, 0); err != nil {
		return request{}, err
	}
	if err := required(opts, "name", "path", "base-ref"); err != nil {
		return request{}, err
	}
	text, err := runner.text(opts["instructions"])
	return request{method: http.MethodPost, path: "/v1/projects", body: core.RegisterProject{Name: opts["name"], Path: opts["path"], BaseRef: opts["base-ref"], Instructions: text}}, err
}

func (runner Runner) tasks(command string, args []string) (request, error) {
	switch command {
	case "create":
		return runner.create(args)
	case "list":
		opts, ids, err := parse(args, "project state", "")
		if err != nil {
			return request{}, err
		}
		if err := positionals(ids, 0); err != nil {
			return request{}, err
		}
		query := url.Values{}
		if opts["project"] != "" {
			query.Set("projectId", opts["project"])
		}
		if opts["state"] != "" {
			query.Set("state", opts["state"])
		}
		return request{method: http.MethodGet, path: withQuery("/v1/tasks", query), table: "tasks"}, nil
	case "show", "followup", "pause", "resume", "cancel", "retry", "accept", "cleanup", "result":
		return runner.taskAction(command, args)
	default:
		return request{}, errors.New("unknown tasks command; use --help")
	}
}

func (runner Runner) create(args []string) (request, error) {
	opts, ids, err := parse(args, "project title instructions role parent depends-on", "")
	if err != nil {
		return request{}, err
	}
	if err := positionals(ids, 0); err != nil {
		return request{}, err
	}
	if err := required(opts, "project", "title", "instructions"); err != nil {
		return request{}, err
	}
	instructions, err := runner.text(opts["instructions"])
	if err != nil {
		return request{}, err
	}
	var dependencies []string
	if value, ok := opts["depends-on"]; ok {
		for _, id := range strings.Split(value, ",") {
			id = strings.TrimSpace(id)
			if err := positionals([]string{id}, 1); err != nil {
				return request{}, errors.New("--depends-on must be a comma-separated list of nonempty IDs")
			}
			dependencies = append(dependencies, id)
		}
	}
	return request{method: http.MethodPost, path: "/v1/tasks", body: core.CreateTask{ProjectID: opts["project"], Title: opts["title"], Instructions: instructions, Role: opts["role"], ParentID: opts["parent"], Dependencies: dependencies}}, nil
}

func (runner Runner) taskAction(command string, args []string) (request, error) {
	values, booleans := "", ""
	switch command {
	case "followup":
		values = "text"
	case "pause", "resume", "cancel", "retry":
		booleans = "cascade"
	case "result":
		values = "attempt generation summary evidence"
	}
	opts, ids, err := parse(args, values, booleans)
	if err != nil {
		return request{}, err
	}
	if err := positionals(ids, 1); err != nil {
		return request{}, err
	}
	path := "/v1/tasks/" + url.PathEscape(ids[0])
	switch command {
	case "show":
		return request{method: http.MethodGet, path: path}, nil
	case "followup":
		if err := required(opts, "text"); err != nil {
			return request{}, err
		}
		text, err := runner.text(opts["text"])
		return request{method: http.MethodPost, path: path + "/followup", body: core.Followup{Text: text}}, err
	case "pause", "resume", "cancel", "retry":
		return request{method: http.MethodPost, path: path + "/control", body: core.Control{Action: command, Cascade: opts["cascade"] == "true"}}, nil
	case "accept", "cleanup":
		return request{method: http.MethodPost, path: path + "/" + command, body: struct{}{}}, nil
	default: // result
		if len(opts) == 0 {
			return request{method: http.MethodGet, path: path + "/results"}, nil
		}
		if err := required(opts, "attempt", "generation", "summary"); err != nil {
			return request{}, err
		}
		generation, err := strconv.Atoi(opts["generation"])
		if err != nil || generation < 1 {
			return request{}, errors.New("--generation must be a positive integer")
		}
		evidence := []core.Evidence{}
		if value, ok := opts["evidence"]; ok {
			text, err := runner.text(value)
			if err != nil {
				return request{}, err
			}
			decoder := json.NewDecoder(strings.NewReader(text))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&evidence) != nil || evidence == nil {
				return request{}, errors.New("--evidence must be a JSON array of name, reference, and passed objects")
			}
			if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
				return request{}, errors.New("--evidence must contain exactly one JSON array")
			}
		}
		return request{method: http.MethodPost, path: path + "/results", body: core.SubmitResult{AttemptID: opts["attempt"], Generation: generation, Summary: opts["summary"], Evidence: evidence}}, nil
	}
}

func (runner Runner) decisions(command string, args []string) (request, error) {
	if command == "list" {
		opts, ids, err := parse(args, "task status", "")
		if err != nil {
			return request{}, err
		}
		if err := positionals(ids, 0); err != nil {
			return request{}, err
		}
		query := url.Values{}
		if opts["task"] != "" {
			query.Set("taskId", opts["task"])
		}
		if opts["status"] != "" {
			query.Set("status", opts["status"])
		}
		return request{method: http.MethodGet, path: withQuery("/v1/decisions", query), table: "decisions"}, nil
	}
	if command != "reply" {
		return request{}, errors.New("unknown decisions command; use --help")
	}
	opts, ids, err := parse(args, "answer", "")
	if err != nil {
		return request{}, err
	}
	if err := positionals(ids, 1); err != nil {
		return request{}, err
	}
	if err := required(opts, "answer"); err != nil {
		return request{}, err
	}
	answer, err := runner.text(opts["answer"])
	if err != nil {
		return request{}, err
	}
	if !json.Valid([]byte(answer)) {
		return request{}, errors.New("--answer must contain exactly one JSON value")
	}
	return request{method: http.MethodPost, path: "/v1/decisions/" + url.PathEscape(ids[0]) + "/reply", body: core.ReplyDecision{Answer: json.RawMessage(answer)}}, nil
}

func withQuery(path string, values url.Values) string {
	if len(values) == 0 {
		return path
	}
	return path + "?" + values.Encode()
}
