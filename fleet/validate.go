package fleet

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Issue is one validation failure. Path is a JSON Pointer into the document, so
// an editor can put the cursor on the offending field rather than on the file.
type Issue struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (i Issue) Error() string { return i.Path + ": " + i.Message }

// varRef matches ${NAME} interpolation references in a declared field.
var varRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ValidateBytes is the ingress (D6). It parses and validates in one call so no
// caller can hold a Spec that never passed the validator. A malformed document
// returns a single /-rooted issue; the secret-leak lint (D4) runs on the raw
// bytes, before anything else, because a leaked credential in an unparseable
// document is still a leaked credential.
func ValidateBytes(b []byte) (*Spec, []Issue) {
	if leaks := LintSecrets(b); len(leaks) > 0 {
		return nil, leaks
	}
	var s Spec
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, []Issue{{Path: "/", Code: "malformed", Message: "not valid JSON: " + err.Error()}}
	}
	return &s, Validate(&s)
}

// Validate returns every issue in the spec, in document order. An empty slice
// means the spec is safe to execute.
func Validate(s *Spec) []Issue {
	var out []Issue
	add := func(path, code, msg string) { out = append(out, Issue{Path: path, Code: code, Message: msg}) }

	if s.Spec == "" {
		add("/spec", "required", "spec is required and must be "+SpecVersion)
	} else if s.Spec != SpecVersion {
		add("/spec", "unsupported_version", "unsupported spec version "+s.Spec+", want "+SpecVersion)
	}
	if strings.TrimSpace(s.Name) == "" {
		add("/name", "required", "name is required")
	}
	if strings.TrimSpace(s.Repo) == "" {
		add("/repo", "required", "repo is required (\".\" for the spec's own repo)")
	}
	// Required with no default, deliberately: an unnamed topology is the first
	// specification failure in a multi-agent run.
	if strings.TrimSpace(s.Topology) == "" {
		add("/topology", "required", "topology is required and has no default — name it")
	}

	declared := map[string]bool{}
	for _, v := range BuiltinVars {
		declared[v] = true
	}
	for i, p := range s.Workspace.Materialise.Ports {
		base := fmt.Sprintf("/workspace/materialise/ports/%d", i)
		if strings.TrimSpace(p.Name) == "" {
			add(base+"/name", "required", "port entry needs a name to bind the allocated port to")
		} else {
			declared[p.Name] = true
		}
		if p.Range[0] <= 0 || p.Range[1] <= 0 || p.Range[0] > p.Range[1] {
			add(base+"/range", "invalid_range", fmt.Sprintf("range must be [low, high] with 0 < low <= high, got [%d, %d]", p.Range[0], p.Range[1]))
		}
	}

	if s.Workspace.Isolation == "" && len(s.Workspace.Materialise.Ports)+len(s.Workspace.Materialise.Files)+
		len(s.Workspace.Materialise.Commands) == 0 && s.Workspace.Materialise.Escape == "" {
		add("/workspace", "required", "workspace is required and must declare an isolation and a materialise block")
	}
	if s.Workspace.Isolation != "" && s.Workspace.Isolation != "worktree" {
		add("/workspace/isolation", "unsupported", "isolation "+s.Workspace.Isolation+" is not supported; only \"worktree\"")
	}

	for i, f := range s.Workspace.Materialise.Files {
		base := fmt.Sprintf("/workspace/materialise/files/%d", i)
		switch {
		case f.From == "" && f.Template == "":
			add(base, "required", "file entry needs exactly one of from (copy) or template (render)")
		case f.From != "" && f.Template != "":
			add(base, "exclusive", "file entry has both from and template; exactly one is allowed")
		}
		if strings.TrimSpace(f.To) == "" {
			add(base+"/to", "required", "file entry needs a destination path")
		}
		if strings.HasPrefix(f.To, "/") || strings.Contains(f.To, "..") {
			add(base+"/to", "escapes_workspace", "destination must stay inside the workspace root")
		}
		out = append(out, undeclaredVars(base+"/to", f.To, declared)...)
		for _, v := range f.Vars {
			if !declared[v] {
				add(base+"/vars", "undeclared_var", "template var "+v+" is not an allocated port or a builtin")
			}
		}
	}
	for i, c := range s.Workspace.Materialise.Commands {
		base := fmt.Sprintf("/workspace/materialise/commands/%d", i)
		if strings.TrimSpace(c.Run) == "" {
			add(base+"/run", "required", "command entry needs a run string")
		}
		out = append(out, undeclaredVars(base+"/run", c.Run, declared)...)
	}
	for i, sv := range s.Workspace.Materialise.Services {
		base := fmt.Sprintf("/workspace/materialise/services/%d", i)
		if strings.TrimSpace(sv.Name) == "" {
			add(base+"/name", "required", "service entry needs a name")
		}
		if strings.TrimSpace(sv.Run) == "" {
			add(base+"/run", "required", "service entry needs a run string")
		}
		if strings.TrimSpace(sv.PortVar) != "" && !declared[sv.PortVar] {
			add(base+"/port_var", "undeclared_var", "port_var "+sv.PortVar+" is not a declared port")
		}
		// Supervision must have something to check. A supervised service with no
		// port would degrade to "assume it is fine", which is the failure this
		// field exists to prevent.
		if sv.Supervise && strings.TrimSpace(sv.PortVar) == "" {
			add(base+"/supervise", "requires_port_var", "supervise needs port_var: without a port there is nothing to check after the probe passes")
		}
		out = append(out, undeclaredVars(base+"/run", sv.Run, declared)...)
	}
	for i, p := range s.Workspace.Materialise.Health {
		base := fmt.Sprintf("/workspace/materialise/health/%d", i)
		switch p.Probe {
		case "http":
			if strings.TrimSpace(p.URL) == "" {
				add(base+"/url", "required", "http probe needs a url")
			}
			out = append(out, undeclaredVars(base+"/url", p.URL, declared)...)
		case "command":
			if strings.TrimSpace(p.Run) == "" {
				add(base+"/run", "required", "command probe needs a run string")
			}
			out = append(out, undeclaredVars(base+"/run", p.Run, declared)...)
		case "":
			add(base+"/probe", "required", "probe kind is required: http or command")
		default:
			add(base+"/probe", "unsupported", "unknown probe kind "+p.Probe+"; want http or command")
		}
	}

	if h := s.Workspace.Materialise.Hooks; h != nil {
		// Ordered, not a map range: Validate promises issues in document order,
		// and Go randomises map iteration.
		for _, hook := range []struct{ field, cmd string }{
			{"on_start", h.OnStart}, {"on_stop", h.OnStop}, {"on_destroy", h.OnDestroy},
		} {
			if hook.cmd == "" {
				continue
			}
			out = append(out, undeclaredVars("/workspace/materialise/hooks/"+hook.field, hook.cmd, declared)...)
		}
	}

	if len(s.Roster) == 0 {
		add("/roster", "required", "roster needs at least one role")
	}
	for i, r := range s.Roster {
		base := fmt.Sprintf("/roster/%d", i)
		if strings.TrimSpace(r.Agent) == "" {
			add(base+"/agent", "required", "role needs an agent")
		}
		if strings.TrimSpace(r.Role) == "" {
			add(base+"/role", "required", "role needs a role name")
		}
		if r.Count < 1 {
			add(base+"/count", "invalid", fmt.Sprintf("count must be at least 1, got %d", r.Count))
		}
		if r.MaxDepth < 0 {
			add(base+"/max_depth", "invalid", "max_depth cannot be negative")
		}
	}

	if s.Budgets != nil {
		switch s.Budgets.OnExceed {
		case "", "drain", "kill":
		default:
			add("/budgets/on_exceed", "unsupported", "on_exceed must be drain or kill, got "+s.Budgets.OnExceed)
		}
	}
	return out
}

// undeclaredVars reports ${VAR} references that resolve against nothing. The
// resolvable set is exactly the allocated ports plus the builtins — no
// environment inheritance, no arbitrary shell expansion in declared fields.
func undeclaredVars(path, s string, declared map[string]bool) []Issue {
	var out []Issue
	for _, m := range varRef.FindAllStringSubmatch(s, -1) {
		if !declared[m[1]] {
			out = append(out, Issue{Path: path, Code: "undeclared_var",
				Message: "${" + m[1] + "} resolves against nothing; declare it as a port or use a builtin"})
		}
	}
	return out
}
