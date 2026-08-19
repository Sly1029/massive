package orchestrator

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/artifact"
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

func TestDescriptorFilePathIncludesEveryOrderedScopeFrame(t *testing.T) {
	root := t.TempDir()
	path, err := descriptorFilePath(root, StepInvocationDescriptor{
		RunID: "run-1", NodeID: "task", Attempt: 1,
		Scope: &ExecutionScope{Frames: []MapItemScopeFrame{
			{Kind: "map-item", MapID: "outer", Index: 0},
			{Kind: "map-item", MapID: "inner", Index: 4},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "run-1", "task", "scopes", "maps", "outer", "items", "0", "maps", "inner", "items", "4", "1.json")
	if path != want {
		t.Fatalf("scoped descriptor path = %q, want %q", path, want)
	}
}

func TestProcessStepInvokerRejectsUnsafeScopeIdentityBeforeWritingDescriptors(t *testing.T) {
	for name, descriptor := range map[string]StepInvocationDescriptor{
		"scope index above JSON safe integer": {
			RunID: "run-1", NodeID: "task", Attempt: 1,
			Scope: &ExecutionScope{Frames: []MapItemScopeFrame{{Kind: "map-item", MapID: "fanout", Index: int(artifact.MaxJSONSafeInteger + 1)}}},
		},
		"attempt above JSON safe integer": {
			RunID: "run-1", NodeID: "task", Attempt: int(artifact.MaxJSONSafeInteger + 1),
		},
	} {
		t.Run(name, func(t *testing.T) {
			descriptorDir := filepath.Join(t.TempDir(), "descriptors")
			_, err := (ProcessStepInvoker{DescriptorDir: descriptorDir, CommandTemplate: []string{"must-not-run"}}).InvokeSteps(t.Context(), StepInvocationBatch{Steps: []StepInvocation{{Descriptor: descriptor}}})
			if err == nil {
				t.Fatal("InvokeSteps accepted unsafe descriptor identity")
			}
			if _, err := os.Stat(descriptorDir); !os.IsNotExist(err) {
				t.Fatalf("unsafe descriptor created directory %q: %v", descriptorDir, err)
			}
		})
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
