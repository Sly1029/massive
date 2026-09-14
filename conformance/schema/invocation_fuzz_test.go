package schema_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Sly1029/massive/conformance/schema/runtimepb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// This exercises the proto-JSON projection. Runner schema validation separately
// enforces the current version, required fields, and invocation semantics.
func FuzzInvocationProjection(f *testing.F) {
	for _, name := range []string{"linear-chain", "map-item"} {
		data, err := os.ReadFile(filepath.Join("..", "fixtures", "descriptors", name, "descriptor.json"))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Add([]byte(`{"kind":"StepInvocationDescriptor","schemaVersion":3,"encoding":"json-v3"}`))
	f.Add([]byte(`{"scope":{"frames":[{"kind":"map-item","mapId":"items","index":4294967295}]}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64*1024 {
			t.Skip()
		}
		var original runtimepb.StepInvocationDescriptor
		if err := protojson.Unmarshal(data, &original); err != nil {
			return
		}
		encoded, err := (protojson.MarshalOptions{EmitDefaultValues: true}).Marshal(&original)
		if err != nil {
			t.Fatal(err)
		}
		var decoded runtimepb.StepInvocationDescriptor
		if err := protojson.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(&original, &decoded) {
			t.Fatal("invocation fields changed across proto-JSON projection")
		}
	})
}
