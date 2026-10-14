package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sly1029/massive/conformance/schema/planpb"
)

func TestLocalSecretResolutionBoundsAndNames(t *testing.T) {
	for _, mode := range []string{"env", "file", "total", "base", "pass", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			value := strings.Repeat("x", maxLocalEnvironmentValueBytes+1)
			if mode == "total" {
				value = strings.Repeat("x", 24<<10)
			}
			if mode == "symlink" {
				value = "token\n"
			}
			t.Setenv("SECRET_SOURCE", value)
			binding := LocalSecretBinding{Env: "SECRET_SOURCE"}
			if mode == "file" || mode == "symlink" {
				path := filepath.Join(t.TempDir(), "secret")
				if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
					t.Fatal(err)
				}
				if mode == "symlink" {
					link := path + ".link"
					if err := os.Symlink(path, link); err != nil {
						t.Skipf("symlink unavailable: %v", err)
					}
					path = link
				}
				binding = LocalSecretBinding{File: path}
			}
			secrets := []*planpb.SecretRef{{Name: "TOKEN", Ref: "ref"}}
			var pass []string
			if mode == "base" {
				secrets[0].Name = "PATH"
			}
			if mode == "pass" {
				secrets[0].Name = "CONFIG"
				pass = []string{"CONFIG"}
				t.Setenv("CONFIG", "safe")
			}
			if mode == "total" {
				for _, name := range []string{"SECOND", "THIRD", "FOURTH", "FIFTH", "SIXTH"} {
					secrets = append(secrets, &planpb.SecretRef{Name: name, Ref: "ref"})
				}
			}
			plan := &planpb.WorkflowPlan{Contracts: []*planpb.ExecutionContract{{ContractRef: "contract", Secrets: secrets}}, Graph: &planpb.GraphIR{Nodes: []*planpb.GraphNode{{Id: "step", Kind: "step", ContractRef: "contract"}}}}
			envs, err := localTaskEnvironments(plan, map[string]LocalSecretBinding{"ref": binding}, pass)
			if mode == "symlink" {
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, entry := range envs["step"] {
					if entry == "TOKEN=token\n" {
						found = true
					}
				}
				if !found {
					t.Fatal("file value was trimmed")
				}
			} else if err == nil {
				t.Fatalf("accepted %s", mode)
			}
		})
	}
}

func TestLocalBindingSchemaRejectsArgoFormatWithBuildDiagnostic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bindings.json")
	if err := os.WriteFile(path, []byte(`{"api":{"name":"secret","key":"token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLocalSecretBindings(path); err == nil || !strings.Contains(err.Error(), "massive build") {
		t.Fatalf("Argo bindings: %v", err)
	}
}
