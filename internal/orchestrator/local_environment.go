package orchestrator

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/deployment"
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
	// Schema diagnostics can render rejected instances, including accidentally
	// supplied literal credentials. Report the expected shape without the data.
	if err := deployment.ValidateLocalSecretBindings(data); err != nil {
		return nil, fmt.Errorf("invalid local secret bindings: expected logical refs mapped to exactly one {\"env\":\"NAME\"} or {\"file\":\"path\"} source; Argo {name,key} bindings belong to massive build")
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

func localBaseEnvironmentNames() []string {
	names := []string{
		"PATH", "HOME", "TMPDIR", "TEMP", "TMP", "LANG", "TZ", "SSL_CERT_FILE", "SSL_CERT_DIR",
		"LC_ALL", "LC_COLLATE", "LC_CTYPE", "LC_MESSAGES", "LC_MONETARY", "LC_NUMERIC", "LC_TIME",
		"MASSIVE_PYTHON", "MASSIVE_TYPESCRIPT_RUNNER",
	}
	if runtime.GOOS == "windows" {
		names = append(names, "SYSTEMROOT", "COMSPEC", "USERPROFILE", "SYSTEMDRIVE", "WINDIR", "PATHEXT", "LOCALAPPDATA", "APPDATA", "USERNAME")
	}
	return names
}

// LocalBaseEnvironment supplies only documented interpreter configuration.
// It returns a non-nil slice, including when no base variables are set.
func LocalBaseEnvironment(pass []string) ([]string, error) {
	names := localBaseEnvironmentNames()
	for _, name := range pass {
		if !portableEnvName.MatchString(name) {
			return nil, fmt.Errorf("invalid --pass-env name %q: use a portable environment variable name", name)
		}
		if _, exists := os.LookupEnv(name); !exists {
			return nil, fmt.Errorf("--pass-env %s is not set in the launching environment", name)
		}
	}
	names = append(names, pass...)
	env := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
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
		}
	}
	if err := validateLocalEnvironmentSize(env); err != nil {
		return nil, err
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
	reservedNames := append(localBaseEnvironmentNames(), pass...)
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
			for _, name := range reservedNames {
				equal := secret.GetName() == name
				if runtime.GOOS == "windows" {
					equal = strings.EqualFold(secret.GetName(), name)
				}
				if equal {
					return nil, fmt.Errorf("secret %s for step %s: name %s conflicts with base or pass-through environment", secret.GetRef(), node.GetId(), name)
				}
			}
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
					file, readErr := os.Open(binding.File)
					if readErr != nil {
						return nil, fmt.Errorf("secret %s for step %s: cannot read source file", ref, node.GetId())
					}
					data, readErr := io.ReadAll(io.LimitReader(file, maxLocalEnvironmentValueBytes+1))
					closeErr := file.Close()
					if readErr == nil {
						readErr = closeErr
					}
					if readErr != nil {
						return nil, fmt.Errorf("secret %s for step %s: cannot read source file", ref, node.GetId())
					}
					value = string(data)
				default:
					return nil, fmt.Errorf("secret %s: specify exactly one env or file source", ref)
				}
				if len(value) > maxLocalEnvironmentValueBytes {
					return nil, fmt.Errorf("secret %s: source exceeds %d bytes", ref, maxLocalEnvironmentValueBytes)
				}
				if strings.ContainsRune(value, 0) {
					return nil, fmt.Errorf("secret %s: source contains NUL, which cannot enter a process environment", ref)
				}
				values[ref] = value
			}
			env = append(env, secret.GetName()+"="+value)
		}
		if err := validateLocalEnvironmentSize(env); err != nil {
			return nil, fmt.Errorf("step %s: %w", node.GetId(), err)
		}
		environments[node.GetId()] = env
	}
	return environments, nil
}

// Leave ample room for argv and platform overhead; Windows counts UTF-16 code
// units, so a 24 KiB UTF-8 budget also stays below its 32,767-unit block limit.
const maxLocalEnvironmentValueBytes = 32 << 10
const maxWindowsEnvironmentBytes = 24 << 10

func validateLocalEnvironmentSize(env []string) error {
	size := 1
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		if len(value) > maxLocalEnvironmentValueBytes {
			return fmt.Errorf("environment variable %s exceeds %d bytes", name, maxLocalEnvironmentValueBytes)
		}
		if strings.ContainsRune(value, 0) {
			return fmt.Errorf("environment variable %s contains NUL", name)
		}
		size += len(entry) + 1
	}
	limit := 128 << 10
	if runtime.GOOS == "windows" {
		limit = maxWindowsEnvironmentBytes
	}
	if size > limit {
		return fmt.Errorf("task environment exceeds %d bytes", limit)
	}
	return nil
}
