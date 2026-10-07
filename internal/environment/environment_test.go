package environment_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

// lockedProject copies the committed locked fixture with an absolute SDK source,
// locks it, and syncs a fresh environment from that lock.
func lockedProject(t *testing.T) (root, prefix string) {
	t.Helper()
	repository := repositoryRoot(t)
	base := sdkPython(t)
	root = t.TempDir()
	fixture := filepath.Join(repository, "conformance", "workflows", "python-locked")
	for _, name := range []string{"workflow.py", "pyproject.toml"} {
		body, err := os.ReadFile(filepath.Join(fixture, name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "pyproject.toml" {
			sdk := filepath.ToSlash(filepath.Join(repository, "packages", "python"))
			body = []byte(strings.Replace(string(body), `"../../../packages/python"`, `"`+sdk+`"`, 1))
		}
		if err := os.WriteFile(filepath.Join(root, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	prefix = filepath.Join(t.TempDir(), "venv")
	run(t, root, nil, "uv", "lock", "--quiet", "--python", base)
	run(t, root, []string{"UV_PROJECT_ENVIRONMENT=" + prefix}, "uv", "sync", "--quiet", "--locked", "--python", base)
	return root, prefix
}

// sdkEnvironment is an environment with only the editable SDK installed.
func sdkEnvironment(t *testing.T) string {
	t.Helper()
	base := sdkPython(t)
	prefix := filepath.Join(t.TempDir(), "venv")
	run(t, "", nil, "uv", "venv", "--quiet", "--python", base, prefix)
	run(t, "", nil, "uv", "pip", "install", "--quiet", "--python", venvPython(prefix), "-e", filepath.Join(repositoryRoot(t), "packages", "python"))
	return venvPython(prefix)
}

func writeProject(t *testing.T, pyproject string, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
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

func check(t *testing.T, python, root, sdkVersion string) *environment.Report {
	t.Helper()
	report, err := environment.Check(t.Context(), environment.Request{Python: python, ProjectRoot: root, SDKVersion: sdkVersion})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestLockedEnvironmentPassesAndRejectsDrift(t *testing.T) {
	root, prefix := lockedProject(t)
	python := venvPython(prefix)

	report := check(t, python, root, "")
	if err := report.Err(); err != nil {
		t.Fatal(err)
	}
	if report.Verification != environment.LockSyncChecked {
		t.Fatalf("verification = %s", report.Verification)
	}
	if report.Interpreter.Prefix != prefix && filepath.Clean(report.Interpreter.Prefix) != filepath.Clean(prefix) {
		t.Fatalf("probed prefix %q, want %q", report.Interpreter.Prefix, prefix)
	}
	if report.Project == nil || strings.Join(report.Project.Dependencies, ",") != "massive-workflows,tabulate<1,>=0.9" {
		t.Fatalf("project requirements = %#v", report.Project)
	}

	t.Run("SDK version must match the control plane", func(t *testing.T) {
		mismatch := check(t, python, root, "9.9.9")
		if got := finding(t, mismatch, environment.SDKVersion); !strings.Contains(got.Message, "9.9.9") {
			t.Fatalf("finding = %#v", got)
		}
		if err := check(t, python, root, "0.1.0").Err(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a lock without uv on PATH fails", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		finding(t, check(t, python, root, ""), environment.UVUnavailable)
	})

	// Exact sync semantics: an extra package fails even when every declared
	// requirement is installed.
	run(t, root, nil, "uv", "pip", "install", "--quiet", "--python", python, "iniconfig")
	drift := check(t, python, root, "")
	if got := codes(drift); len(got) != 1 {
		t.Fatalf("findings = %v, want only the lock mismatch", got)
	}
	extra := finding(t, drift, environment.LockOutOfSync)
	if !strings.Contains(extra.Message, "not in uv.lock iniconfig==") || !strings.Contains(extra.Fix, "must match exactly") {
		t.Fatalf("finding = %#v", extra)
	}

	run(t, root, nil, "uv", "pip", "uninstall", "--quiet", "--python", python, "tabulate")
	missing := check(t, python, root, "")
	finding(t, missing, environment.MissingRequirement)
	if got := finding(t, missing, environment.LockOutOfSync); !strings.Contains(got.Message, "missing tabulate==") {
		t.Fatalf("finding = %#v", got)
	}

	pyproject := filepath.Join(root, "pyproject.toml")
	body, err := os.ReadFile(pyproject)
	if err != nil {
		t.Fatal(err)
	}
	stale := strings.Replace(string(body), `"tabulate>=0.9,<1"`, `"tabulate>=0.9,<1", "iniconfig"`, 1)
	if err := os.WriteFile(pyproject, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	finding(t, check(t, python, root, ""), environment.LockStale)
}

func TestDirectRequirementsWithoutLock(t *testing.T) {
	python := sdkEnvironment(t)
	ready := writeProject(t, `[project]
name = "direct"
version = "0.1.0"
requires-python = ">=3.12"
dependencies = ["massive-workflows", "Pydantic>=2", "absent-package; python_version < '3'"]
`, map[string]string{"workflow.py": ""})
	report := check(t, python, ready, "")
	if err := report.Err(); err != nil {
		t.Fatal(err)
	}
	if report.Verification != environment.DirectRequirementsSatisfied {
		t.Fatalf("verification = %s", report.Verification)
	}

	unsatisfied := writeProject(t, `[project]
name = "direct"
version = "0.1.0"
requires-python = "<3"
dependencies = ["absent-package>=1", "pydantic<1"]
`, map[string]string{"workflow.py": ""})
	report = check(t, python, unsatisfied, "")
	finding(t, report, environment.RequiresPython)
	finding(t, report, environment.MissingRequirement)
	finding(t, report, environment.RequirementVersion)
}

func TestShadowedDistributionsAndModulesFail(t *testing.T) {
	python := sdkEnvironment(t)
	sitePackages := strings.TrimSpace(run(t, "", nil, python, "-I", "-c", "import sysconfig; print(sysconfig.get_path('purelib'))"))
	shadow := filepath.Join(t.TempDir(), "shadow")
	run(t, "", nil, "uv", "pip", "install", "--quiet", "--python", python, "--target", shadow, "--no-deps", "tabulate==0.9.0")
	run(t, "", nil, "uv", "pip", "install", "--quiet", "--python", python, "tabulate==0.10.0")
	if err := os.WriteFile(filepath.Join(sitePackages, "massive-test-shadow.pth"), []byte(shadow+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Importing json.py would write the sentinel; the probe must never do so.
	sentinel := filepath.Join(t.TempDir(), "imported")
	root := writeProject(t, "", map[string]string{
		"json.py":              "open(" + pythonString(sentinel) + ", 'w').close()\n",
		"pydantic/__init__.py": "",
		"workflow.py":          "",
	})
	report := check(t, python, root, "")
	if got := finding(t, report, environment.DuplicateDistribution); !strings.Contains(got.Message, "tabulate") {
		t.Fatalf("finding = %#v", got)
	}
	var shadowed []string
	for _, candidate := range report.Findings {
		if candidate.Code == environment.ShadowedModule {
			shadowed = append(shadowed, candidate.Message)
		}
	}
	if len(shadowed) != 2 || !strings.Contains(shadowed[0], "standard-library module 'json'") || !strings.Contains(shadowed[1], "from pydantic") {
		t.Fatalf("shadowed modules = %q", shadowed)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("planted module was imported: %v", err)
	}
}

func TestInterpreterWithoutSDKCannotBeProbed(t *testing.T) {
	base := strings.TrimSpace(run(t, "", nil, sdkPython(t), "-I", "-c", "import sys; print(sys._base_executable)"))
	root := writeProject(t, "", map[string]string{"workflow.py": ""})
	_, err := environment.Check(t.Context(), environment.Request{Python: base, ProjectRoot: root})
	if err == nil || !strings.Contains(err.Error(), "No module named 'massive'") || !strings.Contains(err.Error(), "uv sync --locked") {
		t.Fatalf("error = %v", err)
	}
}

func pythonString(value string) string {
	return "r'" + value + "'"
}
