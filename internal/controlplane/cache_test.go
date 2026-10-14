package controlplane

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/environment"
)

// Build and install a real wheel: editable development environments deliberately
// bypass these caches. All probes, frontend imports and task processes are real.
func installedCachePython(t *testing.T) string {
	t.Helper()
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	run := func(args ...string) {
		command := exec.Command("uv", args...)
		command.Env = append(os.Environ(), "UV_LINK_MODE=copy")
		if body, err := command.CombinedOutput(); err != nil {
			t.Fatalf("uv %v: %v\n%s", args, err, body)
		}
	}
	run("build", "--wheel", "--out-dir", filepath.Join(root, "wheel"), filepath.Join(repository, "packages", "python"))
	wheels, err := filepath.Glob(filepath.Join(root, "wheel", "*.whl"))
	if err != nil || len(wheels) != 1 {
		t.Fatalf("wheels = %v, %v", wheels, err)
	}
	prefix := filepath.Join(root, "venv")
	// Distro interpreters may have real startup hooks and correctly bypass
	// preparation caches. A hit fixture must select a supported interpreter.
	run("venv", "--quiet", "--managed-python", prefix)
	python := filepath.Join(prefix, "bin", "python")
	if runtime.GOOS == "windows" {
		python = filepath.Join(prefix, "Scripts", "python.exe")
	} else {
		// uv's managed-interpreter aliases can leave lib-dynload on sys.path
		// under an alias even though sys.base_prefix names the resolved tree.
		command := exec.Command(python, "-I", "-c", "import sys; print(sys.base_prefix)")
		base, err := command.Output()
		if err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(root, "base-alias")
		if err := os.Symlink(strings.TrimSpace(string(base)), alias); err != nil {
			t.Fatal(err)
		}
		prefix = filepath.Join(root, "aliased-venv")
		run("venv", "--quiet", "--python", filepath.Join(alias, "bin", "python3"), prefix)
		python = filepath.Join(prefix, "bin", "python")
	}
	run("pip", "install", "--quiet", "--python", python, wheels[0])
	t.Setenv("MASSIVE_PYTHON", python)
	return python
}

func TestLocalPreparationCache(t *testing.T) {
	t.Setenv("UV_LINK_MODE", "copy")
	python := installedCachePython(t)
	// Python processes .pth files only at import roots, not inside package
	// resource directories. This real file must not disable a valid cache.
	resource := exec.Command(python, "-I", "-c", `import site
from pathlib import Path
root = Path(site.getsitepackages()[0]) / "cache_test_resources"
root.mkdir()
(root / "ignored.pth").write_text("import module_that_does_not_exist\n")`)
	if output, err := resource.CombinedOutput(); err != nil {
		t.Fatalf("nested import resource: %v\n%s", err, output)
	}
	root := t.TempDir()
	repository, _ := filepath.Abs(filepath.Join("..", ".."))
	for _, name := range []string{"workflow.py", "helper.py"} {
		body, err := os.ReadFile(filepath.Join(repository, "conformance", "workflows", "python-linear", name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "workflow.py" {
			body = bytes.Replace(body, []byte("class Result(BaseModel):"), []byte("class Result(BaseModel):\n    \"\"\"A <value> & its typed result.\"\"\""), 1)
			body = append(body, []byte("\nother = graph\n")...)
		}
		if err := os.WriteFile(filepath.Join(root, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	entry := filepath.Join(root, "workflow.py") + "#graph"
	storeRoot := writableStoreForTest(t)
	emit := func(selector string, noCache bool, preflight, emission bool) *FrontendResult {
		t.Helper()
		frontend, err := EmitLocal(t.Context(), selector, storeRoot, noCache)
		if err != nil {
			t.Fatal(err)
		}
		if frontend.Cache == nil || frontend.Cache.Preflight != preflight || frontend.Cache.Emission != emission {
			t.Fatalf("cache = %+v, want preflight=%v emission=%v", frontend.Cache, preflight, emission)
		}
		return frontend
	}
	cold := emit(entry, false, false, false)
	warm := emit(entry, false, true, true)
	if !bytes.Equal(cold.Canonical, warm.Canonical) || !bytes.Equal(cold.Compiled.CanonicalJSON, warm.Compiled.CanonicalJSON) {
		t.Fatal("warm preparation differs from cold spec/plan")
	}
	for index, frontend := range []*FrontendResult{cold, warm} {
		id := fmt.Sprintf("run-%d", index)
		result, err := RunLocal(t.Context(), LocalRunRequest{Frontend: frontend, Input: []byte(`{"value":41}`), Store: storeRoot, Project: "test/cache", RunID: id})
		if err != nil {
			t.Fatal(err)
		}
		if string(result.Result) != `{"value":42}` || result.Plan.PlanHash != cold.Compiled.PlanHash {
			t.Fatalf("run = %+v", result)
		}
		journal, _, err := InspectEnvironment(t.Context(), storeRoot, "test/cache", id)
		if err != nil {
			t.Fatal(err)
		}
		if journal.Cache == nil || *journal.Cache != *frontend.Cache {
			t.Fatalf("journal cache = %+v", journal.Cache)
		}
	}
	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: storeRoot})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"emission", "preflight"} {
		objects, err := store.List(t.Context(), datastore.MustKey("cache/local-v1/"+kind))
		if err != nil || len(objects) != 1 {
			t.Fatalf("cache objects: %v, %v", objects, err)
		}
		original, err := store.Get(t.Context(), objects[0].Key)
		if err != nil {
			t.Fatal(err)
		}
		var damaged cacheEntry
		if err := json.Unmarshal(original.Body, &damaged); err != nil {
			t.Fatal(err)
		}
		// Keep a parseable envelope but damage its payload without updating
		// the digest. The reader must verify bytes before using either record.
		damaged.Value = json.RawMessage(`{"corrupt":true}`)
		damagedBytes, err := json.Marshal(damaged)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Put(t.Context(), objects[0].Key, damagedBytes, datastore.PutOptions{ContentType: "application/json"}); err != nil {
			t.Fatal(err)
		}
		emit(entry, true, false, false)
		stillDamaged, err := store.Get(t.Context(), objects[0].Key)
		if err != nil || !bytes.Equal(stillDamaged.Body, damagedBytes) {
			t.Fatalf("--no-cache updated a damaged entry: %v", err)
		}

		repaired := emit(entry, false, kind != "preflight", kind != "emission")
		if repaired.Compiled.PlanHash != cold.Compiled.PlanHash {
			t.Fatal("corrupt entry changed plan identity")
		}
		emit(entry, false, true, true)
	}
	forced := emit(entry, true, false, false)
	if forced.Compiled != nil {
		t.Fatal("uncached emission unexpectedly reused a compiled plan")
	}
	emit(entry, false, true, true)
	emit(filepath.Join(root, "workflow.py")+"#other", false, true, false)

	helper := filepath.Join(root, "helper.py")
	body, _ := os.ReadFile(helper)
	if err := os.WriteFile(helper, append(body, []byte("\n# source changed\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	changed := emit(entry, false, true, false)
	if changed.Compiled.PlanHash == cold.Compiled.PlanHash {
		t.Fatal("source edit did not change plan identity")
	}
	emit(entry, false, true, true)
	// Adding a selected file must also invalidate; checking a cached manifest's
	// existing paths alone would miss new glob matches.
	if err := os.WriteFile(filepath.Join(root, "new.py"), []byte("VALUE = 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	emit(entry, false, false, false)
	emit(entry, false, true, true)
	// Even unselected modules are checked for import shadowing by preflight.
	if err := os.WriteFile(filepath.Join(root, "json.py"), []byte("VALUE = 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := EmitLocal(t.Context(), entry, storeRoot, false); err == nil || !strings.Contains(err.Error(), "SHADOWED_MODULE") {
		t.Fatalf("shadowing = %v", err)
	}
	if err := os.Remove(filepath.Join(root, "json.py")); err != nil {
		t.Fatal(err)
	}
	emit(entry, false, true, true)

	command := exec.Command("uv", "pip", "install", "--quiet", "--python", python, "six==1.16.0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("install distribution: %v\n%s", err, output)
	}
	firstDistribution := emit(entry, false, false, false)
	command = exec.Command("uv", "pip", "install", "--quiet", "--python", python, "six==1.17.0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("change distribution: %v\n%s", err, output)
	}
	secondDistribution := emit(entry, false, false, false)
	if firstDistribution.Environment.Record.GetRealizationHash() == secondDistribution.Environment.Record.GetRealizationHash() {
		t.Fatal("distribution version did not change realization")
	}
	emit(entry, false, true, true)

	// Different executable paths may have the same portable realization; local
	// preparation must nevertheless recheck the interpreter.
	alias := filepath.Join(filepath.Dir(python), "python-cache-alias")
	if runtime.GOOS != "windows" {
		if err := os.Symlink(python, alias); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MASSIVE_PYTHON", alias)
		emit(entry, false, false, false)
		emit(entry, false, true, true)
		t.Setenv("MASSIVE_PYTHON", python)
	}
	pyproject := filepath.Join(root, "pyproject.toml")
	if err := os.WriteFile(pyproject, []byte("[project]\nname = 'cache-test'\nversion = '0.1.0'\nrequires-python = '>=3.12'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	emit(entry, false, false, false)
	emit(entry, false, true, true)
	projectBytes, err := os.ReadFile(pyproject)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pyproject, bytes.ReplaceAll(projectBytes, []byte(">=3.12"), []byte(">=99")), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := EmitLocal(t.Context(), entry, storeRoot, false); err == nil || !strings.Contains(err.Error(), "REQUIRES_PYTHON") {
		t.Fatalf("requires-python = %v", err)
	}
	if err := os.WriteFile(pyproject, projectBytes, 0600); err != nil {
		t.Fatal(err)
	}
	command = exec.Command("uv", "lock", "--offline", "--project", root, "--python", python)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("uv lock: %v\n%s", err, output)
	}
	emit(entry, false, false, false)
	emit(entry, false, true, true)
	lock := filepath.Join(root, "uv.lock")
	body, _ = os.ReadFile(lock)
	if err := os.WriteFile(lock, append(body, []byte("\n# lock changed\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	emit(entry, false, false, false)
	lockedWarm := emit(entry, false, true, true)
	// A changed SDK METADATA is a real installed-distribution mutation, and
	// must be observed even when it retains the same package files.
	command = exec.Command(python, "-I", "-c", `from importlib.metadata import distribution
from pathlib import Path
d = distribution('massive-workflows')
p = next(d.locate_file(f) for f in d.files if str(f).endswith('.dist-info/METADATA'))
s = Path(p).read_text(); Path(p).write_text(s.replace('Version: 0.1.0', 'Version: 0.1.1'))`)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("change SDK metadata: %v\n%s", err, output)
	}
	sdkChanged := emit(entry, false, false, false)
	if sdkChanged.Environment.Record.GetRealizationHash() == lockedWarm.Environment.Record.GetRealizationHash() {
		t.Fatal("SDK version did not change realization")
	}

	// Start concurrent preparations against an empty real store; all entries
	// must remain readable and produce identical plans.
	concurrentStore := writableStoreForTest(t)
	var workers sync.WaitGroup
	errors := make(chan error, 6)
	hashes := make(chan string, 6)
	for index := range 6 {
		workers.Go(func() {
			frontend, err := EmitLocal(t.Context(), entry, concurrentStore, false)
			if err != nil {
				errors <- err
				return
			}
			result, err := RunLocal(t.Context(), LocalRunRequest{Frontend: frontend, Input: []byte(`{"value":41}`), Store: concurrentStore, Project: "test/concurrent-cache", RunID: fmt.Sprintf("run-%d", index)})
			if err != nil {
				errors <- err
				return
			}
			if string(result.Result) != `{"value":42}` {
				errors <- fmt.Errorf("concurrent result = %s", result.Result)
				return
			}
			hashes <- result.Plan.PlanHash
		})
	}
	workers.Wait()
	close(errors)
	close(hashes)
	for err := range errors {
		t.Error(err)
	}
	for hash := range hashes {
		if hash != sdkChanged.Compiled.PlanHash {
			t.Errorf("concurrent plan = %s", hash)
		}
	}
	concurrent, err := EmitLocal(t.Context(), entry, concurrentStore, false)
	if err != nil || concurrent.Cache == nil || !concurrent.Cache.Preflight || !concurrent.Cache.Emission {
		t.Fatalf("concurrent cache: %+v, %v", concurrent, err)
	}
	objects, err := store.List(t.Context(), datastore.MustKey("cache/local-v1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range objects {
		body, err := store.Get(t.Context(), object.Key)
		if err != nil || !json.Valid(body.Body) {
			t.Fatalf("invalid cache object %s: %v", object.Key, err)
		}
	}
	// Moving the same kind of resource to an active site root creates a real
	// startup hook, so both preparations must take the uncached path.
	hook := exec.Command(python, "-I", "-c", `import site
from pathlib import Path
(Path(site.getsitepackages()[0]) / "unsupported.pth").write_text("import sys\n")`)
	if output, err := hook.CombinedOutput(); err != nil {
		t.Fatalf("active import hook: %v\n%s", err, output)
	}
	for range 2 {
		emit(entry, false, false, false)
	}
}

func TestEditableEnvironmentBypassesLocalPreparationCache(t *testing.T) {
	useCancellationPython(t)
	repository, _ := filepath.Abs(filepath.Join("..", ".."))
	entry := filepath.Join(repository, "conformance", "workflows", "python-linear", "workflow.py")
	store := writableStoreForTest(t)
	for range 2 {
		frontend, err := EmitLocal(t.Context(), entry, store, false)
		if err != nil {
			t.Fatal(err)
		}
		if frontend.Cache.Preflight || frontend.Cache.Emission {
			t.Fatalf("editable environment cached: %+v", frontend.Cache)
		}
		if frontend.Environment.Verification != environment.Undeclared {
			t.Fatalf("verification = %s", frontend.Environment.Verification)
		}
	}
}
