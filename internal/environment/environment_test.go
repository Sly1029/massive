package environment_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/environment"
)

// These tests build real environments with uv. Packages come from the uv cache
// or the configured index; the checks themselves always run offline.

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func venvPython(prefix string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(prefix, "Scripts", "python.exe")
	}
	return filepath.Join(prefix, "bin", "python")
}

// sdkPython is the repository's development interpreter, which uv uses as the
// base interpreter for every test environment.
func sdkPython(t *testing.T) string {
	t.Helper()
	python := venvPython(filepath.Join(repositoryRoot(t), "packages", "python", ".venv"))
	if _, err := os.Stat(python); err != nil {
		t.Fatal("install the Python SDK environment with uv sync --project packages/python")
	}
	if _, err := exec.LookPath("uv"); err != nil {
		t.Fatal("environment tests require uv on PATH")
	}
	return python
}

func sdkRelease(t *testing.T) string {
	t.Helper()
	pyproject, err := os.ReadFile(filepath.Join(repositoryRoot(t), "packages", "python", "pyproject.toml"))
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?m)^version = "([^"]+)"$`).FindSubmatch(pyproject)
	if match == nil {
		t.Fatal("packages/python/pyproject.toml declares no version")
	}
	return string(match[1])
}

func run(t *testing.T, dir string, environment []string, name string, args ...string) string {
	t.Helper()
	command := exec.Command(name, args...)
	command.Dir = dir
	command.Env = append(os.Environ(), environment...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, output)
	}
	return string(output)
}

// lockedProject copies the committed locked fixture with an absolute SDK source
// and makes it a packaged project (`uv init --package`) with default dependency
// groups, an optional extra, and a local path dependency, then locks it.
func lockedProject(t *testing.T) string {
	t.Helper()
	repository := repositoryRoot(t)
	root := t.TempDir()
	fixture := filepath.Join(repository, "conformance", "workflows", "python-locked")
	for _, name := range []string{"workflow.py", "pyproject.toml"} {
		body, err := os.ReadFile(filepath.Join(fixture, name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "pyproject.toml" {
			sdk := filepath.ToSlash(filepath.Join(repository, "packages", "python"))
			text := strings.Replace(string(body), `"../../../packages/python"`, `"`+sdk+`"`, 1)
			text = strings.Replace(text, `"tabulate>=0.9,<1"]`, `"tabulate>=0.9,<1", "local-helper"]`, 1)
			text = strings.Replace(text, "\n# Conformance", `
[project.optional-dependencies]
report = ["six"]

[dependency-groups]
dev = ["iniconfig"]
docs = ["six"]

[build-system]
requires = ["hatchling"]
build-backend = "hatchling.build"

[tool.uv]
default-groups = ["dev", "docs"]

# Conformance`, 1)
			text += "local-helper = { path = \"helper\" }\n"
			body = []byte(text)
		}
		if err := os.WriteFile(filepath.Join(root, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeProject(t, filepath.Join(root, "helper"), "[project]\nname = \"local-helper\"\nversion = \"0.1.0\"\n[build-system]\nrequires = [\"hatchling\"]\nbuild-backend = \"hatchling.build\"\n",
		map[string]string{"local_helper.py": "VALUE = 1\n"})
	run(t, root, nil, "uv", "lock", "--quiet", "--python", sdkPython(t))
	return root
}

// syncedEnvironment syncs a fresh environment outside the project's .venv.
func syncedEnvironment(t *testing.T, root string, options ...string) string {
	t.Helper()
	prefix := filepath.Join(t.TempDir(), "venv")
	arguments := append([]string{"sync", "--quiet", "--locked", "--python", sdkPython(t)}, options...)
	run(t, root, []string{"UV_PROJECT_ENVIRONMENT=" + prefix}, "uv", arguments...)
	return prefix
}

// sdkEnvironment is an environment with only the editable SDK installed.
func sdkEnvironment(t *testing.T) string {
	t.Helper()
	prefix := filepath.Join(t.TempDir(), "venv")
	run(t, "", nil, "uv", "venv", "--quiet", "--python", sdkPython(t), prefix)
	run(t, "", nil, "uv", "pip", "install", "--quiet", "--python", venvPython(prefix), "-e", filepath.Join(repositoryRoot(t), "packages", "python"))
	return venvPython(prefix)
}

func writeProject(t *testing.T, root, pyproject string, files map[string]string) string {
	t.Helper()
	if root == "" {
		root = t.TempDir()
	}
	if pyproject != "" {
		files["pyproject.toml"] = pyproject
	}
	for name, body := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func codes(report *environment.Report) []environment.FindingCode {
	var result []environment.FindingCode
	for _, finding := range report.Findings {
		result = append(result, finding.Code)
	}
	return result
}

func finding(t *testing.T, report *environment.Report, code environment.FindingCode) environment.Finding {
	t.Helper()
	for _, candidate := range report.Findings {
		if candidate.Code == code {
			if candidate.Message == "" || candidate.Fix == "" || strings.Contains(candidate.Fix, "\n") {
				t.Fatalf("finding %s lacks a one-line fix: %#v", code, candidate)
			}
			return candidate
		}
	}
	t.Fatalf("findings = %v, want %s", codes(report), code)
	return environment.Finding{}
}

// check uses a source-built control plane unless a release version is given.
func check(t *testing.T, python, root, release string) *environment.Report {
	t.Helper()
	version := release
	if version == "" {
		version = "0.0.0-dev"
	}
	report, err := environment.Check(t.Context(), environment.Request{
		Python: python, ProjectRoot: root, ControlPlaneVersion: version, RequireSDK: release != "",
	})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func ready(t *testing.T, report *environment.Report, verification environment.Verification) {
	t.Helper()
	if err := report.Err(); err != nil {
		t.Fatal(err)
	}
	if report.Verification != verification {
		t.Fatalf("verification = %s, want %s", report.Verification, verification)
	}
}

func TestLockCheckRequiresTheLockedRuntimeSetOnly(t *testing.T) {
	root := lockedProject(t)

	// Like an image: no default groups, and the packaged project itself is
	// never installed because it ships as a source archive.
	t.Run("an image-like environment is ready", func(t *testing.T) {
		ready(t, check(t, venvPython(syncedEnvironment(t, root, "--no-default-groups", "--no-install-project")), root, ""), environment.LockSyncChecked)
	})

	python := venvPython(syncedEnvironment(t, root, "--extra", "report", "--no-install-project"))
	report := check(t, python, root, "")
	ready(t, report, environment.LockSyncChecked)
	if report.Project == nil || strings.Join(report.Project.Dependencies, ",") != "local-helper,massive-workflows,tabulate<1,>=0.9" {
		t.Fatalf("project requirements = %#v", report.Project)
	}

	t.Run("a missing direct reference is named without its URL", func(t *testing.T) {
		other := venvPython(syncedEnvironment(t, root, "--no-install-project"))
		run(t, root, nil, "uv", "pip", "uninstall", "--quiet", "--python", other, "local-helper")
		got := finding(t, check(t, other, root, ""), environment.LockOutOfSync)
		if !strings.Contains(got.Message, "local-helper") || strings.Contains(got.Message, root) || strings.Contains(got.Message, "file:") {
			t.Fatalf("finding = %#v", got)
		}
	})

	t.Run("SDK version must match the control plane", func(t *testing.T) {
		mismatch := check(t, python, root, "9.9.9")
		if got := finding(t, mismatch, environment.SDKVersion); !strings.Contains(got.Message, "9.9.9") || !strings.Contains(got.Fix, "uv lock") {
			t.Fatalf("finding = %#v", got)
		}
		ready(t, check(t, python, root, sdkRelease(t)), environment.LockSyncChecked)
	})

	t.Run("a lock without uv on PATH fails", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		finding(t, check(t, python, root, ""), environment.UVUnavailable)
	})

	t.Run("uv judges the probed interpreter and ignores caller settings", func(t *testing.T) {
		pin := "3.13"
		if strings.HasPrefix(report.Interpreter.Version, "3.13.") {
			pin = "3.12"
		}
		if err := os.WriteFile(filepath.Join(root, ".python-version"), []byte(pin+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(filepath.Join(root, ".python-version"))
		t.Setenv("UV_PYTHON", filepath.Join(t.TempDir(), "missing-python"))
		t.Setenv("UV_NO_DEV", "0")
		t.Setenv("UV_PROJECT_ENVIRONMENT", t.TempDir())
		ready(t, check(t, python, root, ""), environment.LockSyncChecked)
	})

	run(t, root, nil, "uv", "pip", "install", "--quiet", "--python", python, "tabulate==0.9.0")
	replaced := check(t, python, root, "")
	if got := codes(replaced); len(got) != 1 {
		t.Fatalf("findings = %v, want only the lock mismatch", got)
	}
	wrong := finding(t, replaced, environment.LockOutOfSync)
	if !strings.Contains(wrong.Message, "lacks locked runtime packages: tabulate==") || !strings.Contains(wrong.Message, "installed instead: tabulate==0.9.0") {
		t.Fatalf("finding = %#v", wrong)
	}
	// The environment is not the project's .venv, so the fix must name it.
	prefix := report.Interpreter.Prefix
	if !strings.Contains(wrong.Fix, "UV_PROJECT_ENVIRONMENT="+prefix) || !strings.Contains(wrong.Fix, "--no-dev") {
		t.Fatalf("fix does not target the checked environment: %q", wrong.Fix)
	}

	run(t, root, nil, "uv", "pip", "uninstall", "--quiet", "--python", python, "tabulate")
	missing := check(t, python, root, "")
	if got := finding(t, missing, "MISSING_REQUIREMENT"); !strings.Contains(got.Fix, "UV_PROJECT_ENVIRONMENT=") {
		t.Fatalf("finding = %#v", got)
	}
	if got := finding(t, missing, environment.LockOutOfSync); !strings.Contains(got.Message, "tabulate==") {
		t.Fatalf("finding = %#v", got)
	}

	pyproject := filepath.Join(root, "pyproject.toml")
	body, err := os.ReadFile(pyproject)
	if err != nil {
		t.Fatal(err)
	}
	stale := strings.Replace(string(body), `"tabulate>=0.9,<1"`, `"tabulate>=0.9,<1", "packaging"`, 1)
	if err := os.WriteFile(pyproject, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("UV_FROZEN", "1")
	t.Setenv("UV_LOCKED", "0")
	finding(t, check(t, python, root, ""), environment.LockStale)
}

func TestDirectRequirementsWithoutLock(t *testing.T) {
	python := sdkEnvironment(t)
	direct := writeProject(t, "", `[project]
name = "direct"
version = "0.1.0"
requires-python = ">=3.12"
dependencies = ["massive-workflows", "Pydantic>=2", "absent-package; python_version < '3'"]
`, map[string]string{"workflow.py": ""})
	ready(t, check(t, python, direct, ""), environment.DirectRequirementsSatisfied)

	unsatisfied := writeProject(t, "", `[project]
name = "direct"
version = "0.1.0"
requires-python = "<3"
dependencies = ["absent-package>=1", "pydantic<1"]
`, map[string]string{"workflow.py": ""})
	report := check(t, python, unsatisfied, "")
	finding(t, report, "REQUIRES_PYTHON")
	if got := finding(t, report, "MISSING_REQUIREMENT"); !strings.Contains(got.Fix, "uv pip install --python "+python) {
		t.Fatalf("finding = %#v", got)
	}
	finding(t, report, "REQUIREMENT_VERSION")

	undeclared := writeProject(t, "", "", map[string]string{"workflow.py": ""})
	ready(t, check(t, python, undeclared, ""), environment.Undeclared)
}

func TestWorkspaceMemberLockIsNotSilentlyIgnored(t *testing.T) {
	python := sdkEnvironment(t)
	workspace := writeProject(t, "", "[project]\nname = \"root\"\nversion = \"0.1.0\"\n[tool.uv.workspace]\nmembers = [\"members/*\"]\nexclude = [\"members/standalone\"]\n", map[string]string{"uv.lock": "version = 1\n"})
	// The member's own uv.lock would be ignored by uv in favor of the workspace lock.
	member := writeProject(t, filepath.Join(workspace, "members", "flow"), "[project]\nname = \"flow\"\nversion = \"0.1.0\"\n", map[string]string{"workflow.py": "", "uv.lock": "not a lock"})
	if got := finding(t, check(t, python, member, ""), "WORKSPACE_LOCK"); !strings.Contains(got.Message, workspace) {
		t.Fatalf("finding = %#v", got)
	}
	standalone := writeProject(t, filepath.Join(workspace, "members", "standalone"), "[project]\nname = \"standalone\"\nversion = \"0.1.0\"\n", map[string]string{"workflow.py": ""})
	ready(t, check(t, python, standalone, ""), environment.DirectRequirementsSatisfied)
}

// A uv failure is a finding with the exact rerun command, and it keeps every
// probe finding instead of replacing the report with an error.
func TestUVFailureKeepsProbeFindings(t *testing.T) {
	python := sdkEnvironment(t)
	root := writeProject(t, "", "[project]\nname = \"broken\"\nversion = \"0.1.0\"\ndependencies = [\"absent-package\"]\n", map[string]string{"workflow.py": "", "uv.lock": "this is [not a lock"})
	report := check(t, python, root, "")
	finding(t, report, "MISSING_REQUIREMENT")
	failed := finding(t, report, environment.UVFailed)
	if !strings.Contains(failed.Fix, "uv lock --check --offline") || !strings.Contains(failed.Fix, "UV_PROJECT_ENVIRONMENT=") || !strings.Contains(failed.Fix, "--project "+root) {
		t.Fatalf("finding = %#v", failed)
	}
}

func TestShadowedDistributionsAndModulesFail(t *testing.T) {
	python := sdkEnvironment(t)
	sitePackages := strings.TrimSpace(run(t, "", nil, python, "-I", "-c", "import sysconfig; print(sysconfig.get_path('purelib'))"))
	shadow := filepath.Join(t.TempDir(), "shadow")
	run(t, "", nil, "uv", "pip", "install", "--quiet", "--python", python, "--target", shadow, "--no-deps", "tabulate==0.9.0", "iniconfig==2.0.0")
	run(t, "", nil, "uv", "pip", "install", "--quiet", "--python", python, "tabulate==0.10.0", "iniconfig==2.1.0")
	if err := os.WriteFile(filepath.Join(sitePackages, "massive-test-shadow.pth"), []byte(shadow+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Importing json.py would write the sentinel; the probe must never do so.
	sentinel := filepath.Join(t.TempDir(), "imported")
	root := writeProject(t, "", "[project]\nname = \"shadowed\"\nversion = \"0.1.0\"\ndependencies = [\"tabulate\"]\n", map[string]string{
		"json.py":              "open(" + pythonString(sentinel) + ", 'w').close()\n",
		"pydantic/__init__.py": "",
		"workflow.py":          "",
	})
	report := check(t, python, root, "")
	var duplicates, shadowed []string
	for _, candidate := range report.Findings {
		switch candidate.Code {
		case "DUPLICATE_DISTRIBUTION":
			duplicates = append(duplicates, candidate.Message)
		case "SHADOWED_MODULE":
			shadowed = append(shadowed, candidate.Message)
		}
	}
	// iniconfig is not a requirement of this workflow, so its duplicate is irrelevant.
	if len(duplicates) != 1 || !strings.HasPrefix(duplicates[0], "tabulate is installed 2 times") {
		t.Fatalf("duplicates = %q", duplicates)
	}
	if len(shadowed) != 2 || !strings.Contains(shadowed[0], "standard-library module 'json'") || !strings.Contains(shadowed[1], "from pydantic") {
		t.Fatalf("shadowed modules = %q", shadowed)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("planted module was imported: %v", err)
	}
}

func TestProbeErrorsAreConcise(t *testing.T) {
	python := sdkPython(t)
	root := writeProject(t, "", "[project]\nname = \"invalid\"\nversion = \"0.1.0\"\ndependencies = [\"not a requirement!\"]\n", map[string]string{"workflow.py": ""})
	_, err := environment.Check(t.Context(), environment.Request{Python: python, ProjectRoot: root})
	if err == nil || !strings.HasPrefix(err.Error(), "invalid "+filepath.Join(root, "pyproject.toml")+": project.dependencies.0:") ||
		strings.Contains(err.Error(), "errors.pydantic.dev") || strings.Contains(err.Error(), "install") || strings.Contains(err.Error(), "\n") {
		t.Fatalf("error = %v", err)
	}

	base := strings.TrimSpace(run(t, "", nil, python, "-I", "-c", "import sys; print(sys._base_executable)"))
	_, err = environment.Check(t.Context(), environment.Request{Python: base, ProjectRoot: root})
	if err == nil || !strings.Contains(err.Error(), "does not have massive-workflows installed") || !strings.Contains(err.Error(), "uv sync --locked") {
		t.Fatalf("error = %v", err)
	}
}

func pythonString(value string) string {
	return "r'" + value + "'"
}
