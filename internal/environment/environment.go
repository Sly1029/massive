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
	pb "github.com/Sly1029/massive/conformance/schema/materializationpb"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/taskprocess"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"google.golang.org/protobuf/proto"
)

// Verification states how much of the realization was checked.
type Verification string

const (
	// LockSyncChecked means every locked runtime package (no dev groups or
	// extras) is installed at its locked version from a current uv.lock.
	// Additional installed packages are permitted.
	LockSyncChecked Verification = "LOCK_SYNC_CHECKED"
	// DirectRequirementsSatisfied means only requires-python and the applicable
	// [project].dependencies were checked; transitive versions are unconstrained.
	DirectRequirementsSatisfied Verification = "DIRECT_REQUIREMENTS_SATISFIED"
	// Undeclared means the workflow has no [project] metadata, so nothing beyond
	// interpreter safety and the SDK release was verified.
	Undeclared Verification = "UNDECLARED"
)

// FindingCode classifies a preflight failure. The probe contract defines the
// interpreter and project codes; these are added by the control plane.
type FindingCode string

const (
	SDKVersion    FindingCode = "SDK_VERSION"
	UVUnavailable FindingCode = "UV_UNAVAILABLE"
	UVFailed      FindingCode = "UV_FAILED"
	LockStale     FindingCode = "LOCK_STALE"
	LockOutOfSync FindingCode = "LOCK_OUT_OF_SYNC"
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

// Probe is the schema-validated report of `python -I -m massive_environment probe`.
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
	// ControlPlaneVersion is the release of the control plane running the check.
	ControlPlaneVersion string
	// RequireSDK requires massive-workflows to be the ControlPlaneVersion
	// release. A source-built control plane has no release to match.
	RequireSDK bool
}

type Report struct {
	ProjectRoot  string       `json:"projectRoot"`
	Verification Verification `json:"verification"`
	Probe
	// Record identifies the realization; it exists only when there are no findings.
	Record *pb.RealizedEnvironment `json:"-"`
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
// that the lock is current and its runtime packages are installed.
func Check(ctx context.Context, request Request) (*Report, error) {
	probe, err := runProbe(ctx, request.Python, request.ProjectRoot)
	if err != nil {
		return nil, err
	}
	report := &Report{ProjectRoot: request.ProjectRoot, Verification: DirectRequirementsSatisfied, Probe: *probe}
	if probe.Project == nil {
		report.Verification = Undeclared
	}
	lock, err := os.ReadFile(filepath.Join(request.ProjectRoot, "uv.lock"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read uv.lock: %w", err)
	}
	var lockHash *string
	if err == nil {
		lockHash = proto.String(canonical.DigestBytes(lock))
	}
	if request.RequireSDK {
		if finding := sdkVersionFinding(probe, request.ControlPlaneVersion, request.ProjectRoot, lockHash != nil); finding != nil {
			report.Findings = append(report.Findings, *finding)
		}
	}
	if lockHash != nil {
		report.Verification = LockSyncChecked
		report.Findings = append(report.Findings, lockFindings(ctx, request.ProjectRoot, probe.Interpreter)...)
		// A cancelled uv check is not a finding about the environment.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if len(report.Findings) == 0 {
		if report.Record, err = newRecord(report, lockHash, request.ControlPlaneVersion); err != nil {
			return nil, err
		}
	}
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

const probeProjectError = 2

var missingProbe = regexp.MustCompile(`No module named '?massive_environment'?$`)

func runProbe(ctx context.Context, python, root string) (*Probe, error) {
	// Isolated mode keeps the workflow directory and PYTHONPATH off sys.path.
	var stdout, stderr bytes.Buffer
	if err := taskprocess.RunTo(ctx, []string{python, "-I", "-m", "massive_environment", "probe", root}, root, nil, &stdout, &stderr); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var exit *exec.ExitError
		detail := lastLine(stderr.String())
		switch {
		case errors.As(err, &exit) && exit.ExitCode() == probeProjectError:
			// The probe reports unreadable project metadata as one line.
			return nil, errors.New(strings.TrimSpace(stderr.String()))
		case missingProbe.MatchString(detail):
			return nil, fmt.Errorf("%s does not have massive-workflows installed; install it in the project environment with `uv sync --locked`, or launch through `uv run --locked massive`", python)
		case detail == "":
			detail = err.Error()
		}
		return nil, fmt.Errorf("%s cannot run the Massive environment probe: %s", python, detail)
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

func sdkVersionFinding(probe *Probe, version, root string, locked bool) *Finding {
	release := sdkDistribution + "==" + version
	fix := fmt.Sprintf("run `uv pip install --python %s %s`, or launch the massive command installed in that environment", shellQuote(probe.Interpreter.Executable), release)
	if locked {
		fix = fmt.Sprintf("pin %s in [project].dependencies and run `uv lock`, then %s", release, syncCommand(probe.Interpreter.Prefix, root))
	}
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

// uv prints planned changes as " + name==version" lines, or " + name @ url"
// for direct references. Only package names and registry versions are
// reported, never URLs (which can carry credentials) or other uv output.
var plannedChange = regexp.MustCompile(`(?m)^ ([+-]) ([A-Za-z0-9][A-Za-z0-9._-]*)(?:==([A-Za-z0-9.+!_-]+)| @ )`)

// uvSettings may be inherited: they locate the cache and indexes or are
// recorded lock inputs. Every other UV_* variable (UV_FROZEN, UV_NO_SYNC,
// UV_PYTHON, UV_NO_DEV, ...) could change what the check means.
var uvSettings = regexp.MustCompile(`^UV_(CACHE_DIR|NO_CACHE|CONFIG_FILE|NO_CONFIG|INDEX|INDEX_[A-Z0-9_]+|DEFAULT_INDEX|EXTRA_INDEX_URL|FIND_LINKS|INDEX_STRATEGY|KEYRING_PROVIDER|NATIVE_TLS|INSECURE_HOST|EXCLUDE_NEWER|RESOLUTION|PRERELEASE)=`)

func lockFindings(ctx context.Context, root string, interpreter Interpreter) []Finding {
	uv, err := exec.LookPath("uv")
	if err != nil {
		return []Finding{{
			Code:    UVUnavailable,
			Message: "uv.lock is present, but uv is not on PATH to check the environment against it",
			Fix:     "install uv (https://docs.astral.sh/uv/), or launch via `uv run --locked massive …`",
		}}
	}
	environment := []string{"UV_PROJECT_ENVIRONMENT=" + interpreter.Prefix}
	for _, variable := range os.Environ() {
		if (!strings.HasPrefix(variable, "UV_") || uvSettings.MatchString(variable)) && !strings.HasPrefix(variable, "VIRTUAL_ENV=") {
			environment = append(environment, variable)
		}
	}
	// Pin uv to the probed interpreter so .python-version cannot redirect it.
	common := []string{"--offline", "--no-python-downloads", "--project", root, "--python", interpreter.Executable}
	run := func(arguments ...string) (int, string, Finding) {
		arguments = append(arguments, common...)
		var output bytes.Buffer
		err := taskprocess.RunTo(ctx, append([]string{uv}, arguments...), root, environment, &output, &output)
		var exit *exec.ExitError
		switch {
		case err == nil:
			return 0, output.String(), Finding{}
		case errors.As(err, &exit) && exit.ExitCode() == 1:
			return 1, output.String(), Finding{}
		}
		rerun := "UV_PROJECT_ENVIRONMENT=" + shellQuote(interpreter.Prefix) + " uv"
		for _, argument := range arguments {
			rerun += " " + shellQuote(argument)
		}
		return -1, "", Finding{
			Code:    UVFailed,
			Message: fmt.Sprintf("uv could not check uv.lock in %s (%v)", root, err),
			Fix:     fmt.Sprintf("rerun `%s` for uv's diagnostics", rerun),
		}
	}

	switch exit, _, failure := run("lock", "--check"); exit {
	case -1:
		return []Finding{failure}
	case 1:
		return []Finding{{
			Code:    LockStale,
			Message: fmt.Sprintf("uv.lock in %s does not match pyproject.toml", root),
			Fix:     fmt.Sprintf("run `uv lock` in %s and commit the updated uv.lock", root),
		}}
	}
	// The locked runtime set only: dependency groups (including
	// [tool.uv] default-groups), extras, and tools beside it are permitted. The
	// project itself ships as a source archive and is never installed. The SDK
	// is excluded because images install it from a local wheel, whose recorded
	// source differs from the lock; the probe compares its locked version.
	exit, output, failure := run("sync", "--locked", "--check", "--inexact", "--no-default-groups",
		"--no-install-project", "--no-install-package", sdkDistribution)
	switch exit {
	case -1:
		return []Finding{failure}
	case 0:
		return nil
	}
	var required, replaced []string
	for _, match := range plannedChange.FindAllStringSubmatch(output, -1) {
		change := match[2] + " (direct reference)"
		if match[3] != "" {
			change = match[2] + "==" + match[3]
		}
		if match[1] == "+" {
			required = append(required, change)
		} else {
			replaced = append(replaced, change)
		}
	}
	message := fmt.Sprintf("the environment at %s lacks locked runtime packages", interpreter.Prefix)
	if len(required) > 0 {
		message += ": " + strings.Join(required, ", ")
	}
	if len(replaced) > 0 {
		message += " (installed instead: " + strings.Join(replaced, ", ") + ")"
	}
	return []Finding{{Code: LockOutOfSync, Message: message, Fix: syncCommand(interpreter.Prefix, root)}}
}

// syncCommand repairs the probed environment, naming it explicitly unless it
// is the project's default .venv.
func syncCommand(prefix, root string) string {
	if filepath.Clean(prefix) == filepath.Join(root, ".venv") {
		return fmt.Sprintf("run `uv sync --locked` in %s", root)
	}
	return fmt.Sprintf("run `UV_PROJECT_ENVIRONMENT=%s uv sync --locked --no-dev --project %s`, or launch via `uv run --locked massive …` from %s", shellQuote(prefix), shellQuote(root), root)
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9@%+=:,./_-]+$`)

func shellQuote(value string) string {
	if shellSafe.MatchString(value) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func lastLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
