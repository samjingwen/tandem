// Package cli implements the HTTP-only Tandem command client.
// Server startup and scheduler ownership deliberately do not live here.
package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/samjingwen/tandem/internal/client"
)

// Runner permits isolated CLI tests without changing process environment.
type Runner struct {
	In         io.Reader
	Getenv     func(string) string
	HTTPClient *http.Client
}

// Run uses TANDEM_URL, TANDEM_TOKEN, and standard input. Errors are returned to
// main; errOut is reserved for the pre-dispatch operation ID receipt.
func Run(ctx context.Context, args []string, out, errOut io.Writer) error {
	return (Runner{In: os.Stdin, Getenv: os.Getenv}).Run(ctx, args, out, errOut)
}

// Run executes one command. It never starts a service or retries a mutation.
func (runner Runner) Run(ctx context.Context, args []string, out, errOut io.Writer) error {
	if out == nil || errOut == nil {
		return errors.New("CLI output and error writers are required")
	}
	if len(args) == 0 {
		_, err := io.WriteString(out, usage)
		return err
	}
	getenv := runner.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	options, rest, err := globalOptions(args)
	if err != nil {
		return err
	}
	if options["help"] == "true" || (len(rest) == 1 && rest[0] == "help") {
		_, err := io.WriteString(out, usage)
		return err
	}
	if options["url"] == "" {
		options["url"] = getenv("TANDEM_URL")
	}
	request, err := runner.command(rest)
	if err != nil {
		return err
	}
	httpClient, err := client.New(client.Options{URL: options["url"], Token: getenv("TANDEM_TOKEN"), HTTPClient: runner.HTTPClient})
	if err != nil {
		return err
	}
	op := options["operation-id"]
	if request.method == http.MethodPost {
		if op == "" {
			var random [16]byte
			if _, err := rand.Read(random[:]); err != nil {
				return errors.New("operation ID could not be generated; supply --operation-id")
			}
			op = "op_" + hex.EncodeToString(random[:])
		}
		if err := client.ValidateOperationID(op); err != nil {
			return err
		}
		// This must succeed before any network call. stdout stays machine-readable.
		receipt := "Operation ID: " + op + "\n"
		n, err := io.WriteString(errOut, receipt)
		if err == nil && n != len(receipt) {
			err = io.ErrShortWrite
		}
		if err != nil {
			return fmt.Errorf("write operation ID before dispatch: %w", err)
		}
	} else if op != "" {
		return errors.New("--operation-id applies only to mutations")
	}
	var response json.RawMessage
	if err := httpClient.Do(ctx, request.method, request.path, op, request.body, &response); err != nil {
		return err
	}
	if options["json"] != "true" && request.table != "" {
		return printTable(out, request.table, response)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, response, "", "  "); err != nil {
		return errors.New("server response could not be formatted")
	}
	pretty.WriteByte('\n')
	_, err = out.Write(pretty.Bytes())
	return err
}

type request struct {
	method string
	path   string
	body   any
	table  string
}

// globalOptions accepts global flags before or after command arguments.
func globalOptions(args []string) (map[string]string, []string, error) {
	options := map[string]string{}
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		name, value, hasValue := strings.Cut(strings.TrimPrefix(args[i], "--"), "=")
		if args[i] == "--help" || args[i] == "-h" {
			options["help"] = "true"
			continue
		}
		if !strings.HasPrefix(args[i], "--") || (name != "url" && name != "json" && name != "operation-id") {
			rest = append(rest, args[i])
			// A command option owns its following value, even when the text
			// happens to look like a global flag (for example --text --help).
			if strings.HasPrefix(args[i], "--") && !hasValue && commandValueOption(name) && i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
			continue
		}
		if _, exists := options[name]; exists {
			return nil, nil, fmt.Errorf("--%s may only be supplied once", name)
		}
		if name == "json" {
			if !hasValue {
				value = "true"
			}
			if value != "true" && value != "false" {
				return nil, nil, errors.New("--json expects true or false")
			}
		} else if !hasValue {
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "--") {
				return nil, nil, fmt.Errorf("--%s requires a value", name)
			}
			value = args[i]
		}
		if name != "json" && value == "" {
			return nil, nil, fmt.Errorf("--%s requires a nonempty value", name)
		}
		options[name] = value
	}
	return options, rest, nil
}

func commandValueOption(name string) bool {
	switch name {
	case "name", "path", "base-ref", "instructions", "project", "title", "role", "parent", "depends-on", "text", "attempt", "summary", "evidence", "task", "status", "answer", "state":
		return true
	default:
		return false
	}
}

func printTable(out io.Writer, kind string, data json.RawMessage) error {
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(data, &rows); err != nil {
		return errors.New("server list response must be an array")
	}
	columns := []string{"id", "name", "path", "baseRef"}
	if kind == "tasks" {
		columns = []string{"id", "projectId", "state", "desired", "title"}
	} else if kind == "decisions" {
		columns = []string{"id", "taskId", "kind", "status", "prompt"}
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, strings.ToUpper(strings.Join(columns, "\t"))); err != nil {
		return err
	}
	for _, row := range rows {
		values := make([]string, 0, len(columns))
		for _, column := range columns {
			var value string
			_ = json.Unmarshal(row[column], &value)
			// Untrusted task titles and prompts must not control the terminal.
			value = strings.Map(func(r rune) rune {
				if unicode.IsControl(r) {
					return ' '
				}
				return r
			}, value)
			values = append(values, value)
		}
		if _, err := fmt.Fprintln(table, strings.Join(values, "\t")); err != nil {
			return err
		}
	}
	return table.Flush()
}

const usage = `Tandem HTTP client

Global flags: --url URL --json --operation-id ID
Environment: TANDEM_URL (default http://127.0.0.1:7433), TANDEM_TOKEN

projects register --name NAME --path PATH --base-ref REF [--instructions TEXT]
projects list
tasks create --project ID --title TITLE --instructions TEXT [--role ROLE] [--parent ID] [--depends-on ID,ID]
tasks list [--project ID] [--state STATE]
tasks show ID
tasks followup ID --text TEXT
tasks pause|resume|cancel ID [--cascade]
tasks accept|cleanup ID
tasks result ID
tasks result ID --attempt ID --summary TEXT [--evidence JSON_ARRAY]
decisions list [--task ID] [--status STATUS]
decisions reply ID --answer JSON
snapshot
doctor
version

Use '-' to read instructions, followup text, answer, or evidence from stdin.
Every mutation prints its operation ID to stderr before dispatch.
Retain that ID and the exact request body for any manual retry.
No mutation is automatically retried. No command starts a scheduler.
`
