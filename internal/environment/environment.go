// Package environment checks that an existing Python interpreter realizes a
// workflow project's declared requirements before any workflow module is
// imported. It records facts about the realization; it never installs anything.
package environment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	contract "github.com/Sly1029/massive/conformance/schema"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Verification states how much of the realization was checked.
type Verification string

const (
	// LockSyncChecked means `uv sync --locked --check` found the interpreter's
	// environment exactly synchronized with a current uv.lock.
	LockSyncChecked Verification = "LOCK_SYNC_CHECKED"
	// DirectRequirementsSatisfied means only requires-python and the applicable
	// [project].dependencies were checked; transitive versions are unconstrained.
	DirectRequirementsSatisfied Verification = "DIRECT_REQUIREMENTS_SATISFIED"
)

// FindingCode classifies a preflight failure. Probe codes come from the Python
// probe contract; the remaining codes are added here.
type FindingCode string

const (
	RequiresPython        FindingCode = "REQUIRES_PYTHON"
	MissingRequirement    FindingCode = "MISSING_REQUIREMENT"
	RequirementVersion    FindingCode = "REQUIREMENT_VERSION"
	DuplicateDistribution FindingCode = "DUPLICATE_DISTRIBUTION"
	ShadowedModule        FindingCode = "SHADOWED_MODULE"
	SDKVersion            FindingCode = "SDK_VERSION"
	UVUnavailable         FindingCode = "UV_UNAVAILABLE"
	LockStale             FindingCode = "LOCK_STALE"
	LockOutOfSync         FindingCode = "LOCK_OUT_OF_SYNC"
)

// Finding is one problem with a one-line fix. Messages never contain raw uv
// output: index URLs in that output can carry credentials.
type Finding struct {
	Code    FindingCode `json:"code"`
	Message string      `json:"message"`
	Fix     string      `json:"fix"`
}

type Interpreter struct {
	Executable     string  `json:"executable"`
	Prefix         string  `json:"prefix"`
	Implementation string  `json:"implementation"`
	Version        string  `json:"version"`
	CacheTag       *string `json:"cacheTag"`
	Platform       string  `json:"platform"`
	OS             string  `json:"os"`
	Arch           string  `json:"arch"`
}

type ProjectRequirements struct {
	RequiresPython *string  `json:"requiresPython"`
	Dependencies   []string `json:"dependencies"`
}

type Distribution struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Direct   bool   `json:"direct"`
	Editable bool   `json:"editable"`
}

// Probe is the schema-validated report of `python -I -m massive.environment probe`.
type Probe struct {
	SchemaVersion int                  `json:"schemaVersion"`
	Interpreter   Interpreter          `json:"interpreter"`
	Project       *ProjectRequirements `json:"project"`
	Distributions []Distribution       `json:"distributions"`
	Findings      []Finding            `json:"findings"`
}

type Request struct {
	// Python is the interpreter to inspect; the report pins its sys.executable.
	Python string
	// ProjectRoot is the workflow directory holding pyproject.toml and uv.lock.
	ProjectRoot string
	// SDKVersion is the massive-workflows release paired with this control
	// plane. A source-built control plane leaves it empty.
	SDKVersion string
}

type Report struct {
	ProjectRoot  string       `json:"projectRoot"`
	Verification Verification `json:"verification"`
	Probe
}

// Err reports every finding at once so one fix cycle can address them all.
func (report *Report) Err() error {
	if len(report.Findings) == 0 {
		return nil
	}
	return &PreflightError{ProjectRoot: report.ProjectRoot, Findings: report.Findings}
}

type PreflightError struct {
	ProjectRoot string
	Findings    []Finding
}

func (e *PreflightError) Error() string {
	var message strings.Builder
	fmt.Fprintf(&message, "dependency preflight failed for %s; no workflow module was imported", e.ProjectRoot)
	for _, finding := range e.Findings {
		fmt.Fprintf(&message, "\n  %s: %s\n    fix: %s", finding.Code, finding.Message, finding.Fix)
	}
	return message.String()
}

const sdkDistribution = "massive-workflows"

// Check probes the interpreter and, when the project has a uv.lock, verifies
// that the lock is current and the interpreter's environment matches it exactly.
func Check(ctx context.Context, request Request) (*Report, error) {
	probe, err := runProbe(ctx, request.Python, request.ProjectRoot)
	if err != nil {
		return nil, err
	}
	report := &Report{ProjectRoot: request.ProjectRoot, Verification: DirectRequirementsSatisfied, Probe: *probe}
	if request.SDKVersion != "" {
		if finding := sdkVersionFinding(probe, request.SDKVersion); finding != nil {
			report.Findings = append(report.Findings, *finding)
		}
	}
	if _, err := os.Stat(filepath.Join(request.ProjectRoot, "uv.lock")); errors.Is(err, os.ErrNotExist) {
		return report, nil
	} else if err != nil {
		return nil, fmt.Errorf("inspect uv.lock: %w", err)
	}
	report.Verification = LockSyncChecked
	findings, err := lockFindings(ctx, request.ProjectRoot, probe.Interpreter.Prefix)
	if err != nil {
		return nil, err
	}
	report.Findings = append(report.Findings, findings...)
	return report, nil
}

var probeSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(contract.EnvironmentProbeSchemaJSON))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("environment-probe.schema.json", document); err != nil {
		return nil, err
	}
	return compiler.Compile("environment-probe.schema.json")
})

func runProbe(ctx context.Context, python, root string) (*Probe, error) {
	// Isolated mode keeps the workflow directory and PYTHONPATH off sys.path.
	command := exec.CommandContext(ctx, python, "-I", "-m", "massive.environment", "probe", root)
	command.Dir = root
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		detail := lastLine(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("%s cannot run the Massive environment probe (%s); install massive-workflows in the project environment with `uv sync --locked`, or launch through `uv run --locked massive`", python, detail)
	}
	schema, err := probeSchema()
	if err != nil {
		return nil, fmt.Errorf("compile environment probe schema: %w", err)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		return nil, fmt.Errorf("environment probe emitted invalid JSON: %w", err)
	}
	if err := schema.Validate(value); err != nil {
		return nil, fmt.Errorf("environment probe output does not match this control plane; install the matching massive-workflows release: %w", err)
	}
	var probe Probe
	if err := json.Unmarshal(stdout.Bytes(), &probe); err != nil {
		return nil, err
	}
	return &probe, nil
}

func sdkVersionFinding(probe *Probe, version string) *Finding {
	fix := fmt.Sprintf("pin %s==%s in [project].dependencies and run `uv lock && uv sync --locked`, or launch the massive command installed in that environment", sdkDistribution, version)
	for _, distribution := range probe.Distributions {
		if distribution.Name != sdkDistribution {
			continue
		}
		if distribution.Version == version {
			return nil
		}
		return &Finding{
			Code:    SDKVersion,
			Message: fmt.Sprintf("%s %s in %s does not match the massive %s control plane", sdkDistribution, distribution.Version, probe.Interpreter.Executable, version),
			Fix:     fix,
		}
	}
	return &Finding{
		Code:    SDKVersion,
		Message: fmt.Sprintf("%s is not installed as a distribution in %s", sdkDistribution, probe.Interpreter.Executable),
		Fix:     fix,
	}
}

// uv prints planned changes as " + name==version" lines; only those package
// names and versions are reported, never other uv output.
var plannedChange = regexp.MustCompile(`(?m)^ ([+-]) ([A-Za-z0-9][A-Za-z0-9._-]*)==([A-Za-z0-9.+!_-]+)`)

func lockFindings(ctx context.Context, root, prefix string) ([]Finding, error) {
	uv, err := exec.LookPath("uv")
	if err != nil {
		return []Finding{{
			Code:    UVUnavailable,
			Message: "uv.lock is present, but uv is not on PATH to check the environment against it",
			Fix:     "install uv (https://docs.astral.sh/uv/), or launch via `uv run --locked massive …`",
		}}, nil
	}
	environment := append(os.Environ(), "UV_PROJECT_ENVIRONMENT="+prefix, "UV_PYTHON_DOWNLOADS=never")
	lockCheck := exec.CommandContext(ctx, uv, "lock", "--check", "--offline", "--project", root)
	lockCheck.Dir, lockCheck.Env = root, environment
	if exit, err := runUV(lockCheck, &bytes.Buffer{}); err != nil {
		return nil, err
	} else if exit != 0 {
		return []Finding{{
			Code:    LockStale,
			Message: fmt.Sprintf("uv.lock in %s does not match pyproject.toml", root),
			Fix:     fmt.Sprintf("run `uv lock` in %s and commit the updated uv.lock", root),
		}}, nil
	}
	var output bytes.Buffer
	syncCheck := exec.CommandContext(ctx, uv, "sync", "--locked", "--check", "--offline", "--project", root)
	syncCheck.Dir, syncCheck.Env = root, environment
	if exit, err := runUV(syncCheck, &output); err != nil {
		return nil, err
	} else if exit == 0 {
		return nil, nil
	}
	var install, remove []string
	for _, match := range plannedChange.FindAllStringSubmatch(output.String(), -1) {
		change := match[2] + "==" + match[3]
		if match[1] == "+" {
			install = append(install, change)
		} else {
			remove = append(remove, change)
		}
	}
	var differences []string
	if len(install) > 0 {
		differences = append(differences, "missing "+strings.Join(install, ", "))
	}
	if len(remove) > 0 {
		differences = append(differences, "not in uv.lock "+strings.Join(remove, ", "))
	}
	message := fmt.Sprintf("the environment at %s does not match uv.lock", prefix)
	if len(differences) > 0 {
		message += ": " + strings.Join(differences, "; ")
	}
	fix := fmt.Sprintf("run `uv sync --locked` in %s", root)
	if len(remove) > 0 {
		fix += "; it removes packages missing from uv.lock because a locked environment must match exactly (declare needed packages in [project].dependencies and run `uv lock`)"
	}
	return []Finding{{Code: LockOutOfSync, Message: message, Fix: fix}}, nil
}

// runUV returns uv's check verdict: exit 1 means "not current". Any other
// failure is an error that names the command to rerun, without uv's output.
func runUV(command *exec.Cmd, output *bytes.Buffer) (int, error) {
	command.Stdout, command.Stderr = output, output
	err := command.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return 1, nil
	default:
		return 0, fmt.Errorf("`uv %s` failed in %s (%v); rerun it there for details", strings.Join(command.Args[1:4], " "), command.Dir, err)
	}
}

func lastLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
