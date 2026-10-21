package opencode

import "github.com/samjingwen/tandem/core"

type permissionRule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

// The pinned FileAccess service emits Location-relative internal paths, absolute
// external paths, and ../ paths for other checkouts inside the same project.
// Session rules follow agent rules, and the last matching rule wins.
// The thin plugin additionally guards canonical paths/tool entrypoints and forces
// fresh shell/approval asks, which saved project approvals would otherwise bypass.
func sessionPermissions(spec core.SessionSpec) []permissionRule {
	rules := []permissionRule{
		{"*", "*", "deny"},
		{"execute", "*", "allow"},
		{"read", "*", "allow"},
		{"read", "*.env", "deny"},
		{"read", "*.env.*", "deny"},
		{"read", "*.env.example", "allow"},
		{"glob", "*", "allow"},
		// Grep authorizes a regex, not each returned file, in this release.
		{"grep", "*", "ask"},
		{"question", "*", "allow"},
		{"shell", "*", "ask"},
		{"external_directory", "*", "deny"},
		{"subagent", "*", "deny"},
		{"opencode_*", "*", "deny"},
	}
	for _, name := range []string{"tandem_create", "tandem_status", "tandem_followup", "tandem_decision", "tandem_result"} {
		rules = append(rules, permissionRule{name, "*", "allow"})
	}
	if spec.Role == "coordinator" {
		rules = append(rules, permissionRule{"edit", "*", "deny"}, permissionRule{"tandem.permission.reply", "*", "ask"})
	} else {
		rules = append(rules, permissionRule{"edit", "*", "allow"}, permissionRule{"edit", "*.env", "deny"}, permissionRule{"edit", "*.env.*", "deny"}, permissionRule{"edit", "*.env.example", "allow"})
	}
	for _, action := range []string{"read", "edit"} {
		if action == "edit" && spec.Role == "coordinator" {
			continue
		}
		for _, resource := range []string{"..", "../*", "/*", "?:/*"} {
			rules = append(rules, permissionRule{action, resource, "deny"})
		}
	}
	return rules
}
