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
	sourcePackageRoot := t.TempDir()

	argv, err := DefaultRunnerCommand(DefaultRunnerCommandInputs{
		WorkingDir:        workingDir,
		DescriptorDir:     descriptorDir,
		DatastoreRoot:     datastoreRoot,
		SourcePackageRoot: sourcePackageRoot,
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
			mustAbs(t, sourcePackageRoot),
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

func TestDefaultRunnerCommandOmitsSourcePackageRootInsideWorkingDir(t *testing.T) {
	workingDir := t.TempDir()
	sourcePackageRoot := filepath.Join(workingDir, "workflow")

	argv, err := DefaultRunnerCommand(DefaultRunnerCommandInputs{
		WorkingDir:        workingDir,
		DescriptorDir:     t.TempDir(),
		DatastoreRoot:     t.TempDir(),
		SourcePackageRoot: sourcePackageRoot,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, arg := range argv {
		if strings.HasPrefix(arg, "--allow-read=") && strings.Contains(arg, sourcePackageRoot) {
			t.Fatalf("allow-read includes source package root inside working dir: %q", arg)
		}
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
