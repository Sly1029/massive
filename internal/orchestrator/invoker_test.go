package orchestrator

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDefaultRunnerCommandScopesDenoPermissions(t *testing.T) {
	workingDir := t.TempDir()
	descriptorDir := t.TempDir()
	datastoreRoot := t.TempDir()
	argv, err := DefaultRunnerCommand(DefaultRunnerCommandInputs{
		Language:      "typescript",
		WorkingDir:    workingDir,
		DescriptorDir: descriptorDir,
		DatastoreRoot: datastoreRoot,
	})
	if err != nil {
		t.Fatal(err)
	}

	expected := []string{
		"deno",
		"run",
		"--config",
		"deno.json",
		"--allow-read=" + strings.Join([]string{
			mustAbs(t, workingDir),
			mustAbs(t, descriptorDir),
			mustAbs(t, datastoreRoot),
		}, ","),
		"--allow-write=" + mustAbs(t, datastoreRoot),
		"packages/sdk/src/runner/main.ts",
		descriptorPathToken,
	}
	if !reflect.DeepEqual(argv, expected) {
		t.Fatalf("argv = %#v, want %#v", argv, expected)
	}
	for _, arg := range argv {
		if arg == "--allow-read" || arg == "--allow-write" || strings.HasPrefix(arg, "--allow-env") {
			t.Fatalf("argv contains unscoped permission %q: %#v", arg, argv)
		}
	}
}

func TestDefaultRunnerCommandSelectsPythonRunner(t *testing.T) {
	workingDir := t.TempDir()
	argv, err := DefaultRunnerCommand(DefaultRunnerCommandInputs{
		Language:      "python",
		WorkingDir:    workingDir,
		DescriptorDir: t.TempDir(),
		DatastoreRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"uv",
		"run",
		"--project",
		filepath.Join(mustAbs(t, workingDir), "packages", "python"),
		"--frozen",
		"massive-python-runner",
		descriptorPathToken,
	}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv = %#v, want %#v", argv, want)
	}
}

func TestDefaultRunnerCommandRejectsUnsupportedLanguage(t *testing.T) {
	_, err := DefaultRunnerCommand(DefaultRunnerCommandInputs{
		Language:      "ruby",
		WorkingDir:    t.TempDir(),
		DescriptorDir: t.TempDir(),
		DatastoreRoot: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), `unsupported runner language "ruby"`) {
		t.Fatalf("error = %v, want unsupported runner language", err)
	}
}

func TestSubstituteDescriptorPathKeepsTemplateBehavior(t *testing.T) {
	descriptorPath := filepath.Join(t.TempDir(), "descriptor.json")

	replaced := substituteDescriptorPath([]string{"runner", descriptorPathToken, "--flag"}, descriptorPath)
	expectedReplaced := []string{"runner", descriptorPath, "--flag"}
	if !reflect.DeepEqual(replaced, expectedReplaced) {
		t.Fatalf("replaced argv = %#v, want %#v", replaced, expectedReplaced)
	}

	appended := substituteDescriptorPath([]string{"runner", "--flag"}, descriptorPath)
	expectedAppended := []string{"runner", "--flag", descriptorPath}
	if !reflect.DeepEqual(appended, expectedAppended) {
		t.Fatalf("appended argv = %#v, want %#v", appended, expectedAppended)
	}
}

func mustAbs(t *testing.T, path string) string {
	t.Helper()

	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return absolute
}
