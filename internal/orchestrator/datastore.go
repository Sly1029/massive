package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"

	schemacontract "github.com/Sly1029/massive/conformance/schema"
	"github.com/Sly1029/massive/internal/datastore"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ParseDatastoreDescriptor accepts the same credential-free union used by
// language runners. Deployment config cannot smuggle credentials or options
// that only one of the runtimes understands.
func ParseDatastoreDescriptor(data []byte) (DatastoreDescriptor, error) {
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemacontract.StepInvocationDescriptorSchemaJSON))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("descriptor.json", document); err != nil {
		return nil, err
	}
	schema, err := compiler.Compile("descriptor.json#/$defs/datastore")
	if err != nil {
		return nil, err
	}
	if err := schema.Validate(instance); err != nil {
		return nil, fmt.Errorf("invalid datastore configuration; provide a local or S3 descriptor without credentials: %w", err)
	}
	var tag struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &tag); err != nil {
		return nil, err
	}
	if tag.Kind == "local" {
		var local LocalDatastoreDescriptor
		if err := json.Unmarshal(data, &local); err != nil {
			return nil, err
		}
		local.Path, err = filepath.Abs(local.Path)
		return local, err
	}
	var remote S3DatastoreDescriptor
	if err := json.Unmarshal(data, &remote); err != nil {
		return nil, err
	}
	return remote, nil
}

func openInvocationDatastore(ctx context.Context, descriptor DatastoreDescriptor) (datastore.Datastore, error) {
	switch value := descriptor.(type) {
	case LocalDatastoreDescriptor:
		return datastore.NewLocalDatastore(datastore.LocalConfig{Root: value.Path})
	case S3DatastoreDescriptor:
		endpoint := value.Endpoint
		if endpoint == "" {
			endpoint = "https://s3." + value.Region + ".amazonaws.com"
		}
		address, err := url.Parse(endpoint)
		if err != nil || address.Host == "" || (address.Scheme != "https" && address.Scheme != "http") || address.User != nil || address.RawQuery != "" || address.Fragment != "" || (address.Path != "" && address.Path != "/") {
			return nil, fmt.Errorf("S3 endpoint must be an HTTP(S) origin without credentials, path, query or fragment")
		}
		return datastore.NewS3Datastore(ctx, datastore.S3Config{Endpoint: address.Host, Secure: address.Scheme == "https", Bucket: value.Bucket, Region: value.Region, Prefix: value.Prefix, ForcePathStyle: value.ForcePathStyle})
	default:
		return nil, fmt.Errorf("isolated invocation requires an explicit datastore descriptor")
	}
}
