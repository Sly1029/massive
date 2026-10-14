package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/runevent"
)

func TestLocalTaskEnvironmentBindings(t *testing.T) {
	binary, python, _ := progressCLI(t)
	root := t.TempDir()
	entry := filepath.Join(root, "workflow.py")
	source := `import os
import hashlib
from pathlib import Path
from massive import GraphBuilder, StepContext, container, execution
image = container("example.invalid/runner@sha256:" + "0" * 64)
def plain(ctx: StepContext[str]) -> bool:
    assert os.getenv("UNDECLARED_CREDENTIAL") is None
    assert os.getenv("BOUND_TOKEN") is None
    assert os.getenv("BOUND_FILE") is None
    assert os.getenv("CI_TOKEN_SOURCE") is None
    assert os.getenv("MASSIVE_UNDECLARED_CREDENTIAL") is None
    assert os.getenv("VIRTUAL_ENV") is None
    assert os.getenv("LOCAL_CONFIG") == "enabled"
    Path(ctx.inputs + ".plain").touch()
    return True
def authenticated(ctx: StepContext[bool]) -> bool:
    assert ctx.inputs
    assert hashlib.sha256(os.environ["BOUND_TOKEN"].encode()).hexdigest() == "a8a961bbfd48e245639ee53f297e279f9d7dca11931034975265c0803ba9fd14"
    assert hashlib.sha256(os.environ["BOUND_FILE"].encode()).hexdigest() == "16072921ed76d02c1de47e75e56e7f0fd00dfa65eff93395ed84cd9abc50705e"
    assert os.getenv("CI_TOKEN_SOURCE") is None
    assert os.getenv("UNDECLARED_CREDENTIAL") is None
    return True
graph = GraphBuilder(name="scoped", input_type=str, output_type=bool, defaults=execution(environment=image))
a = graph.add(plain)
b = graph.add(authenticated, contract=execution(environment=image, secrets={"BOUND_TOKEN":"api", "BOUND_FILE":"file"}))
graph.edge_from(graph.start).to(a)
graph.edge_from(a).to(b)
graph.edge_from(b).to_end(graph.end)
`
	if err := os.WriteFile(entry, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	bindings := filepath.Join(root, "bindings.json")
	if err := os.WriteFile(bindings, []byte(`{"api":{"env":"CI_TOKEN_SOURCE"},"file":{"file":"token"},"unused":{"env":"DOES_NOT_EXIST"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "token"), []byte("bound-file-value-never-journal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "MASSIVE_PYTHON="+python, "UNDECLARED_CREDENTIAL=hidden-value", "MASSIVE_UNDECLARED_CREDENTIAL=hidden-value", "CI_TOKEN_SOURCE=bound-env-value-never-journal", "LOCAL_CONFIG=enabled", "VIRTUAL_ENV=unused-activation", "BOUND_TOKEN=ambient-bait", "BOUND_FILE=ambient-bait")
	for _, mode := range []string{"bound", "missing-binding", "missing-env", "missing-file", "invalid-source", "missing-pass-env", "folded-missing-pass-env"} {
		t.Run(mode, func(t *testing.T) {
			store, marker, events := progressStore(t), filepath.Join(root, mode+"-started"), filepath.Join(root, mode+".ndjson")
			input, _ := json.Marshal(marker)
			args := []string{"run", entry, "--store", store, "--project", "tests/local-bindings", "--run-id", mode, "--json", "--progress", "none", "--events", events, "--pass-env", "LOCAL_CONFIG"}
			args = append(args, "--input", string(input))
			if mode == "missing-pass-env" {
				args = append(args, "--pass-env", "DOES_NOT_EXIST")
			}
			if mode == "folded-missing-pass-env" {
				args = append(args, "--pass-env", "tZ")
			}
			if mode != "missing-binding" {
				body := `{"api":{"env":"CI_TOKEN_SOURCE"},"file":{"file":"token"},"unused":{"env":"DOES_NOT_EXIST"}}`
				if mode == "missing-env" {
					body = `{"api":{"env":"DOES_NOT_EXIST"},"file":{"file":"token"}}`
				}
				if mode == "missing-file" {
					body = `{"api":{"env":"CI_TOKEN_SOURCE"},"file":{"file":"missing-token"}}`
				}
				if mode == "invalid-source" {
					body = `{"api":{"value":"bound-env-value-never-journal"}}`
				}
				if err := os.WriteFile(bindings, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--secret-bindings", bindings)
			}
			command := exec.Command(binary, args...)
			command.Env = env
			if mode == "folded-missing-pass-env" {
				command.Env = make([]string, 0, len(env))
				for _, variable := range env {
					name, _, _ := strings.Cut(variable, "=")
					if !strings.EqualFold(name, "TZ") {
						command.Env = append(command.Env, variable)
					}
				}
			}
			output, err := command.CombinedOutput()
			if mode == "bound" {
				if err != nil {
					t.Fatalf("run: %v\n%s", err, output)
				}
				if !bytes.Contains(output, []byte(`"result":true`)) {
					t.Fatalf("result: %s", output)
				}
			} else {
				want := "secret"
				if mode == "missing-pass-env" || mode == "folded-missing-pass-env" {
					want = "--pass-env"
				}
				if err == nil || !bytes.Contains(output, []byte(want)) {
					t.Fatalf("missing/invalid source: %s %v", output, err)
				}
				if _, err := os.Stat(marker + ".plain"); !os.IsNotExist(err) {
					t.Fatalf("step ran before secret validation: %v", err)
				}
			}
			for _, value := range []string{"bound-env-value-never-journal", "bound-file-value-never-journal", "hidden-value"} {
				if bytes.Contains(output, []byte(value)) {
					t.Fatal("CLI diagnostic leaked a secret")
				}
				// Scan all serialized artifacts, not just journal/events.
				if err := filepath.WalkDir(store, func(path string, item os.DirEntry, err error) error {
					if os.IsNotExist(err) {
						return nil
					}
					if err != nil {
						return err
					}
					if item.IsDir() {
						return nil
					}
					body, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					if bytes.Contains(body, []byte(value)) {
						return fmt.Errorf("binding value persisted in %s", path)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				body, _ := os.ReadFile(events)
				if bytes.Contains(body, []byte(value)) {
					t.Fatal("event leaked a secret")
				}
			}
		})
	}
}

// Each item records its author-code lifetime in a separate file. Overlap between
// distinct maps proves ready branches execute together; their combined sweep
// measures the run-wide budget independently of the implementation's semaphore.
func TestLocalWorkerBudgetAcrossIndependentMaps(t *testing.T) {
	binary, python, _ := progressCLI(t)
	root := t.TempDir()
	entry := filepath.Join(root, "workflow.py")
	source := `import os
import time
import json
from pathlib import Path
from massive import GraphBuilder, StepContext, container, execution

def source(ctx: StepContext[list[int]]) -> list[int]:
    return ctx.inputs
def first(ctx: StepContext[int]) -> int:
    return measure("first", ctx.inputs)
def second(ctx: StepContext[int]) -> int:
    return measure("second", ctx.inputs)
def measure(branch: str, value: int) -> int:
    started = time.time_ns()
    root = Path(os.environ["LIFETIME_DIR"])
    if value == 0 and int(os.environ["WORKER_LIMIT"]) > 1:
        (root / (branch + ".ready")).touch()
        other = root / (("second" if branch == "first" else "first") + ".ready")
        deadline = time.monotonic() + 15
        while not other.exists() and time.monotonic() < deadline:
            time.sleep(0.01)
        assert other.exists(), "independent map did not dispatch"
    time.sleep(0.3)
    ended = time.time_ns()
    Path(os.environ["LIFETIME_DIR"], f"{branch}-{value}.json").write_text(json.dumps({"branch":branch, "start":started, "end":ended}))
    return value

graph = GraphBuilder(name="workers", input_type=list[int], output_type=list[int], defaults=execution(environment=container("example.invalid/runner@sha256:" + "0"*64)))
producer = graph.add(source)
graph.edge_from(graph.start).to(producer)
a = graph.map(producer, first, id="first-map", concurrency=2)
b = graph.map(producer, second, id="second-map", concurrency=2)
graph.edge_from(a).to_end(graph.end)
`
	// The otherwise unused branch must reach the graph's end through a typed merge.
	source = strings.Replace(source, "graph.edge_from(a).to_end(graph.end)", `def merge(ctx: StepContext[list[list[int]]]) -> list[int]:
    return ctx.inputs[0] + ctx.inputs[1]
joined = graph.add(merge)
graph.gather(a, b).to(joined)
graph.edge_from(joined).to_end(graph.end)`, 1)
	if err := os.WriteFile(entry, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			lifetimes := filepath.Join(root, fmt.Sprintf("lifetimes-%d", limit))
			if err := os.Mkdir(lifetimes, 0o700); err != nil {
				t.Fatal(err)
			}
			events := filepath.Join(root, fmt.Sprintf("processes-%d.ndjson", limit))
			command := exec.Command(binary, "run", entry, "--events", events, "--store", progressStore(t), "--project", "tests/workers", "--json", "--progress", "none", "--input", "[0,1,2,3]", "--pass-env", "LIFETIME_DIR", "--pass-env", "WORKER_LIMIT", "--max-workers", fmt.Sprint(limit))
			command.Env = append(os.Environ(), "MASSIVE_PYTHON="+python, "LIFETIME_DIR="+lifetimes, "WORKER_LIMIT="+fmt.Sprint(limit))
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("run: %v\n%s", err, output)
			}
			if !bytes.Contains(output, []byte(`"result":[0,1,2,3,0,1,2,3]`)) {
				t.Fatalf("unordered result: %s", output)
			}
			// Starts precede interpreter startup; finishes follow process-group and
			// pipe cleanup. These observations enclose each complete process lifetime.
			stream, err := os.ReadFile(events)
			if err != nil {
				t.Fatal(err)
			}
			active, processMaximum, starts := 0, 0, 0
			for _, line := range bytes.Split(bytes.TrimSpace(stream), []byte("\n")) {
				event, err := runevent.Parse(line)
				if err != nil {
					t.Fatal(err)
				}
				if event.ItemIndex == nil {
					continue
				} // Map collection is not a process.
				switch event.Type {
				case "step.attempt.started":
					active++
					starts++
				case "step.attempt.finished":
					active--
				}
				processMaximum = max(processMaximum, active)
				if active < 0 || active > limit {
					t.Fatalf("live process observations = %d, limit = %d", active, limit)
				}
			}
			if active != 0 || starts != 8 || processMaximum != limit {
				t.Fatalf("process lifetimes: active=%d starts=%d maximum=%d", active, starts, processMaximum)
			}
			type interval struct {
				Branch     string
				Start, End int64
			}
			var intervals []interval
			files, err := os.ReadDir(lifetimes)
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range files {
				if !strings.HasSuffix(file.Name(), ".json") {
					continue
				}
				body, err := os.ReadFile(filepath.Join(lifetimes, file.Name()))
				if err != nil {
					t.Fatal(err)
				}
				var interval interval
				if err := json.Unmarshal(body, &interval); err != nil {
					t.Fatal(err)
				}
				intervals = append(intervals, interval)
			}
			if len(intervals) != 8 {
				t.Fatalf("lifetimes = %d", len(intervals))
			}
			type change struct {
				at    int64
				delta int
			}
			var changes []change
			crossBranch := false
			for _, a := range intervals {
				changes = append(changes, change{a.Start, 1}, change{a.End, -1})
				for _, b := range intervals {
					if a.Branch != b.Branch && a.Start < b.End && b.Start < a.End {
						crossBranch = true
					}
				}
			}
			sort.Slice(changes, func(i, j int) bool {
				if changes[i].at == changes[j].at {
					return changes[i].delta < changes[j].delta
				}
				return changes[i].at < changes[j].at
			})
			live, maximum := 0, 0
			for _, change := range changes {
				live += change.delta
				maximum = max(maximum, live)
			}
			if maximum != limit {
				t.Fatalf("maximum overlapping processes = %d, want %d", maximum, limit)
			}
			if limit > 1 && !crossBranch {
				t.Fatal("independent maps never overlapped")
			}
			// Per-map concurrency also bounds each branch, even with a larger run limit.
			for _, branch := range []string{"first", "second"} {
				for _, a := range intervals {
					count := 0
					for _, b := range intervals {
						if b.Branch == branch && b.Start <= a.Start && a.Start < b.End {
							count++
						}
					}
					if count > 2 {
						t.Fatalf("%s exceeded map concurrency", branch)
					}
				}
			}
		})
	}
}

func TestLocalSchedulerDispatchesDependentsOnCompletion(t *testing.T) {
	binary, python, _ := progressCLI(t)
	root := t.TempDir()
	entry := filepath.Join(root, "workflow.py")
	source := `import os, time
from pathlib import Path
from massive import GraphBuilder, StepContext, container, execution

def source(ctx: StepContext[str]) -> str:
    return ctx.inputs

def slow(ctx: StepContext[str]) -> str:
    root = Path(ctx.inputs)
    (root / "a-started").touch()
    deadline = time.monotonic() + 10
    while not (root / "c-started").exists() and time.monotonic() < deadline:
        time.sleep(0.01)
    assert (root / "c-started").exists(), "dependent waited for slow sibling"
    (root / "a-finished").touch()
    return ctx.inputs

def fast(ctx: StepContext[str]) -> str:
    root = Path(ctx.inputs)
    deadline = time.monotonic() + 10
    while not (root / "a-started").exists() and time.monotonic() < deadline:
        time.sleep(0.01)
    assert (root / "a-started").exists()
    return ctx.inputs

def dependent(ctx: StepContext[str]) -> str:
    root = Path(ctx.inputs)
    assert not (root / "a-finished").exists()
    (root / "c-started").touch()
    return ctx.inputs

def merge(ctx: StepContext[list[str]]) -> list[str]:
    return ctx.inputs

graph = GraphBuilder(name="completion", input_type=str, output_type=list[str], defaults=execution(environment=container("example.invalid/runner@sha256:" + "0"*64)))
producer = graph.add(source)
a = graph.add(slow)
b = graph.add(fast)
c = graph.add(dependent)
d = graph.add(merge)
graph.edge_from(graph.start).to(producer)
graph.edge_from(producer).to(a)
graph.edge_from(producer).to(b)
graph.edge_from(b).to(c)
graph.gather(a, c).to(d)
graph.edge_from(d).to_end(graph.end)
`
	if err := os.WriteFile(entry, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(root)
	command := exec.Command(binary, "run", entry, "--store", progressStore(t), "--project", "tests/completion", "--input", string(input), "--max-workers", "2", "--progress", "none", "--json")
	command.Env = append(os.Environ(), "MASSIVE_PYTHON="+python)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("completion scheduling: %v\n%s", err, output)
	}
}

func TestLocalExactEnvironmentForStepMapAndExitHook(t *testing.T) {
	binary, python, _ := progressCLI(t)
	root := t.TempDir()
	entry := filepath.Join(root, "workflow.py")
	source := `import json, os, sys
from pathlib import Path
from massive import GraphBuilder, StepContext, RunOutcome, container, execution

def check() -> None:
    expected = set(json.loads(os.environ["EXPECTED_KEYS"])) | {"TOKEN"}
    # CoreFoundation can add this variable during macOS interpreter startup.
    # Its generated uid/encoding tuple must replace the launching bait value.
    if sys.platform == "darwin" and "__CF_USER_TEXT_ENCODING" in os.environ:
        uid, script, region = os.environ["__CF_USER_TEXT_ENCODING"].split(":")
        assert int(uid, 16) == os.getuid()
        assert int(script, 16) >= 0 and int(region, 16) >= 0
        expected.add("__CF_USER_TEXT_ENCODING")
    assert set(os.environ) == expected, f"environment keys: {set(os.environ) ^ expected}"
    assert os.environ["TOKEN"] == "secret\n"

def first(ctx: StepContext[list[int]]) -> list[int]:
    check()
    return ctx.inputs

def item(ctx: StepContext[int]) -> int:
    check()
    return ctx.inputs

def hook(ctx: StepContext[RunOutcome]) -> None:
    check()
    Path(os.environ["HOOK_MARKER"]).touch()

contract = execution(environment=container("example.invalid/runner@sha256:" + "0"*64), secrets={"TOKEN":"api"})
graph = GraphBuilder(name="exact-env", input_type=list[int], output_type=list[int], defaults=contract)
a = graph.add(first)
graph.edge_from(graph.start).to(a)
b = graph.map(a, item, id="items")
graph.edge_from(b).to_end(graph.end)
graph.on_exit(hook)
`
	if err := os.WriteFile(entry, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	bindings := filepath.Join(root, "bindings.json")
	if err := os.WriteFile(bindings, []byte(`{"api":{"env":"SECRET_SOURCE"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "hook-started")
	// Independent contract assertion: enumerate the documented base, then intersect
	// it with the launching environment rather than reusing the resolver under test.
	allowed := strings.Fields("PATH HOME TMPDIR TEMP TMP LANG TZ SSL_CERT_FILE SSL_CERT_DIR LC_ALL LC_COLLATE LC_CTYPE LC_MESSAGES LC_MONETARY LC_NUMERIC LC_TIME MASSIVE_PYTHON MASSIVE_TYPESCRIPT_RUNNER")
	if runtime.GOOS == "windows" {
		allowed = append(allowed, strings.Fields("SYSTEMROOT COMSPEC USERPROFILE SYSTEMDRIVE WINDIR PATHEXT LOCALAPPDATA APPDATA USERNAME")...)
	}
	env := append(os.Environ(), "MASSIVE_PYTHON="+python, "SECRET_SOURCE=secret\n", "SSL_CERT_FILE="+filepath.Join(root, "ca.pem"), "SSL_CERT_DIR="+root, "LC_ALL=C.UTF-8", "HOOK_MARKER="+marker, "__CF_USER_TEXT_ENCODING=ambient-bait")
	expected := []string{"EXPECTED_KEYS", "HOOK_MARKER"}
	for _, key := range allowed {
		for _, entry := range env {
			name, _, _ := strings.Cut(entry, "=")
			if name == key || runtime.GOOS == "windows" && strings.EqualFold(name, key) {
				expected = append(expected, key)
				break
			}
		}
	}
	keys, _ := json.Marshal(expected)
	env = append(env, "EXPECTED_KEYS="+string(keys))
	command := exec.Command(binary, "run", entry, "--store", progressStore(t), "--project", "tests/exact-env", "--input", "[1,2]", "--secret-bindings", bindings, "--pass-env", "EXPECTED_KEYS", "--pass-env", "HOOK_MARKER", "--max-workers", "2", "--json", "--progress", "none")
	command.Env = env
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("exact environments: %v\n%s", err, output)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("exit hook did not pass exact environment assertion: %v\n%s", err, output)
	}
}

func TestLocalMissingInactiveBranchSecretFailsBeforeDispatch(t *testing.T) {
	binary, python, _ := progressCLI(t)
	root := t.TempDir()
	entry := filepath.Join(root, "workflow.py")
	source := `from pathlib import Path
from typing import Annotated, Literal
from pydantic import BaseModel, Field
from massive import GraphBuilder, StepContext, container, execution

class Active(BaseModel):
    kind: Literal["active"] = "active"
class Inactive(BaseModel):
    kind: Literal["inactive"] = "inactive"
Choice = Annotated[Active | Inactive, Field(discriminator="kind")]

def classify(ctx: StepContext[str]) -> Choice:
    Path(ctx.inputs).touch()
    return Active()
def active(ctx: StepContext[Active]) -> bool:
    return True
def inactive(ctx: StepContext[Inactive]) -> bool:
    return False

image = container("example.invalid/runner@sha256:" + "0"*64)
graph = GraphBuilder(name="inactive-secret", input_type=str, output_type=bool, defaults=execution(environment=image))
a = graph.add(classify)
b = graph.add(active)
c = graph.add(inactive, contract=execution(environment=image, secrets={"TOKEN":"inactive-only"}))
graph.edge_from(graph.start).to(a)
route = graph.decision(a, on="kind", id="route")
graph.edge_from(route.case(Active)).to(b)
graph.edge_from(route.case(Inactive)).to(c)
selected = route.select(bool, active=b, inactive=c)
graph.edge_from(selected).to_end(graph.end)
`
	for _, mode := range []string{"missing", "reserved-name"} {
		t.Run(mode, func(t *testing.T) {
			workflow := source
			if mode == "reserved-name" {
				workflow = strings.Replace(workflow, `"TOKEN":"inactive-only"`, `"PATH":"inactive-only"`, 1)
			}
			if err := os.WriteFile(entry, []byte(workflow), 0o600); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, mode+"-dispatched")
			input, _ := json.Marshal(marker)
			command := exec.Command(binary, "run", entry, "--store", progressStore(t), "--project", "tests/inactive-secret", "--input", string(input), "--progress", "none")
			command.Env = append(os.Environ(), "MASSIVE_PYTHON="+python)
			output, err := command.CombinedOutput()
			if err == nil || !bytes.Contains(output, []byte("inactive-only")) {
				t.Fatalf("inactive secret: %v\n%s", err, output)
			}
			if mode == "reserved-name" && !bytes.Contains(output, []byte("conflicts")) {
				t.Fatalf("reserved secret name: %s", output)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("dispatched before inactive secret validation: %v", err)
			}
		})
	}
}
