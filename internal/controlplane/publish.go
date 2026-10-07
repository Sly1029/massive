package controlplane

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Sly1029/massive/conformance/schema/materializationpb"
	"github.com/Sly1029/massive/conformance/schema/planpb"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/orchestrator"
	"github.com/Sly1029/massive/internal/target/argo"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// PublishArgoSources uploads an object-store-v0 bundle's verified source
// archives to the datastore its pods read. Each file must match the digest
// recorded in bundle-manifest.json, and each archive must match the digest
// pinned by the bundle's materialization manifest and WorkflowTemplate.
func PublishArgoSources(ctx context.Context, bundleDirectory string, descriptor orchestrator.DatastoreDescriptor) ([]orchestrator.PublishedSourceArchive, error) {
	var manifest planpb.TargetBundleManifest
	if err := readBundleJSON(bundleDirectory, "bundle-manifest.json", "", &manifest); err != nil {
		return nil, err
	}
	// SchemaVersion has explicit presence: a manifest without one is obsolete.
	if manifest.GetTarget() != argo.Kind || manifest.SchemaVersion == nil || manifest.GetSchemaVersion() != argo.BundleManifestSchemaVersion {
		return nil, fmt.Errorf("bundle-manifest.json is not a current Argo bundle; rebuild it with massive build")
	}
	if transport := manifest.GetRuntimeTransport(); transport != argo.TransportObjectStore {
		return nil, fmt.Errorf("bundle uses runtime transport %q, which carries source in its runtime ConfigMap; only %s bundles are published", transport, argo.TransportObjectStore)
	}
	digests := make(map[string]string, len(manifest.GetFiles()))
	for _, file := range manifest.GetFiles() {
		digests[file.GetPath()] = file.GetArtifact().GetHash()
	}
	if digests["materialization-manifest.json"] == "" {
		return nil, fmt.Errorf("bundle-manifest.json does not record materialization-manifest.json; rebuild it with massive build")
	}
	var materialization materializationpb.MaterializationManifest
	if err := readBundleJSON(bundleDirectory, "materialization-manifest.json", digests["materialization-manifest.json"], &materialization); err != nil {
		return nil, err
	}
	archives := make(map[string][]byte, len(materialization.GetSourceArchives()))
	for _, source := range materialization.GetSourceArchives() {
		name, err := orchestrator.SourceArchiveBundleName(source.GetPackageHash())
		if err != nil {
			return nil, err
		}
		path := "runtime-assets/" + name
		body, err := os.ReadFile(filepath.Join(bundleDirectory, filepath.FromSlash(path)))
		if err != nil {
			return nil, fmt.Errorf("read bundle source archive: %w", err)
		}
		if actual := canonical.DigestBytes(body); actual != digests[path] || actual != source.GetArchiveHash() {
			return nil, fmt.Errorf("bundle file %s has digest %s; bundle-manifest.json records %s and the materialization manifest pins %s", path, actual, digests[path], source.GetArchiveHash())
		}
		archives[source.GetPackageHash()] = body
	}
	return orchestrator.PublishSourceArchives(ctx, descriptor, archives)
}

// readBundleJSON decodes one bundle file; a nonempty digest must match its bytes.
func readBundleJSON(bundleDirectory, name, digest string, message proto.Message) error {
	body, err := os.ReadFile(filepath.Join(bundleDirectory, name))
	if err != nil {
		return fmt.Errorf("read bundle %s: %w", name, err)
	}
	if digest != "" && canonical.DigestBytes(body) != digest {
		return fmt.Errorf("bundle %s does not match the digest in bundle-manifest.json", name)
	}
	if err := protojson.Unmarshal(body, message); err != nil {
		return fmt.Errorf("decode bundle %s; rebuild it with massive build: %w", name, err)
	}
	return nil
}
