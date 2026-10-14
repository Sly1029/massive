package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/Sly1029/massive/conformance/schema/planpb"
)

// LocalSecretBinding names a launching-machine source, never its value.
// Exactly one source is required by the CLI's JSON schema.
type LocalSecretBinding struct {
	Env  string `json:"env,omitempty"`
	File string `json:"file,omitempty"`
}

// ReadLocalSecretBindings keeps the logical-ref map used by Argo, with local
// sources instead of Kubernetes keys. Relative paths belong to the input file.
func ReadLocalSecretBindings(path string) (map[string]LocalSecretBinding, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read local secret bindings: %w", err)
	}
	const schema = `{"type":"object","propertyNames":{"minLength":1},"additionalProperties":{"type":"object","additionalProperties":false,"properties":{"env":{"type":"string","pattern":"^[A-Za-z_][A-Za-z0-9_]*$"},"file":{"type":"string","minLength":1}},"oneOf":[{"required":["env"]},{"required":["file"]}]}}`
	// Schema diagnostics can render rejected instances, including accidentally
	// supplied literal credentials. Report the expected shape without the data.
	if err := validateJSONAgainstSchema(schema, data); err != nil {
		return nil, fmt.Errorf("invalid local secret bindings: expected logical refs mapped to exactly one {\"env\":\"NAME\"} or {\"file\":\"path\"} source")
	}
	var bindings map[string]LocalSecretBinding
	if err := json.Unmarshal(data, &bindings); err != nil {
		return nil, err
	}
	for ref, binding := range bindings {
		if binding.File != "" && !filepath.IsAbs(binding.File) {
			binding.File = filepath.Join(filepath.Dir(path), binding.File)
			bindings[ref] = binding
		}
	}
	return bindings, nil
}

var portableEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// LocalBaseEnvironment is deliberately small. Interpreter selection is already
// pinned by preflight; activation and interpreter-injection variables are absent.
// It always returns a non-nil slice so an empty environment cannot inherit.
func LocalBaseEnvironment(pass []string) ([]string, error) {
	names := []string{
		"PATH", "HOME", "TMPDIR", "TEMP", "TMP", "LANG", "TZ",
		"LC_ALL", "LC_COLLATE", "LC_CTYPE", "LC_MESSAGES", "LC_MONETARY", "LC_NUMERIC", "LC_TIME",
		"MASSIVE_PYTHON", "MASSIVE_TYPESCRIPT_RUNNER",
	}
	if runtime.GOOS == "windows" {
		names = append(names, "SYSTEMROOT", "COMSPEC", "USERPROFILE")
	}
	names = append(names, pass...)
	env := make([]string, 0, len(names))
	explicit := make(map[string]bool, len(pass))
	for _, name := range pass {
		explicit[name] = true
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if !portableEnvName.MatchString(name) {
			return nil, fmt.Errorf("invalid --pass-env name %q: use a portable environment variable name", name)
		}
		key := name
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(key)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if value, exists := os.LookupEnv(name); exists {
			env = append(env, name+"="+value)
		} else if explicit[name] {
			return nil, fmt.Errorf("--pass-env %s is not set in the launching environment", name)
		}
	}
	return env, nil
}

// localTaskEnvironments resolves only executable nodes' requirements, including
// inactive branches and the exit hook, before the run dispatches any process.
// Values live only in memory and the process environment, outside artifacts.
func localTaskEnvironments(plan *planpb.WorkflowPlan, bindings map[string]LocalSecretBinding, pass []string) (map[string][]string, error) {
	base, err := LocalBaseEnvironment(pass)
	if err != nil {
		return nil, err
	}
	contracts := make(map[string]*planpb.ExecutionContract)
	for _, contract := range plan.GetContracts() {
		contracts[contract.GetContractRef()] = contract
	}
	environments := make(map[string][]string)
	values := make(map[string]string)
	nodes := append([]*planpb.GraphNode{}, plan.GetGraph().GetNodes()...)
	if hook := plan.GetGraph().GetExitHook(); hook != nil {
		nodes = append(nodes, hook)
	}
	for _, node := range nodes {
		if node.GetKind() != "step" && node.GetKind() != "map" {
			continue
		}
		env := append(make([]string, 0, len(base)), base...)
		for _, secret := range contracts[node.GetContractRef()].GetSecrets() {
			ref := secret.GetRef()
			value, resolved := values[ref]
			if !resolved {
				binding, exists := bindings[ref]
				if !exists {
					return nil, fmt.Errorf("step %s requires secret %s (%s); provide --secret-bindings", node.GetId(), ref, secret.GetName())
				}
				switch {
				case binding.Env != "" && binding.File == "":
					value, exists = os.LookupEnv(binding.Env)
					if !exists {
						return nil, fmt.Errorf("secret %s for step %s: source environment variable %s is not set", ref, node.GetId(), binding.Env)
					}
				case binding.File != "" && binding.Env == "":
					data, readErr := os.ReadFile(binding.File)
					if readErr != nil {
						return nil, fmt.Errorf("secret %s for step %s: cannot read source file", ref, node.GetId())
					}
					value = string(data)
				default:
					return nil, fmt.Errorf("secret %s: specify exactly one env or file source", ref)
				}
				if strings.ContainsRune(value, 0) {
					return nil, fmt.Errorf("secret %s: source contains NUL, which cannot enter a process environment", ref)
				}
				values[ref] = value
			}
			// Declared bindings override an explicit pass-through of the same name.
			env = append(env, secret.GetName()+"="+value)
		}
		environments[node.GetId()] = env
	}
	return environments, nil
}
