// Package artifact publishes and resolves immutable canonical JSON values.
// A body is content-addressed first; its run-scoped manifest is the commit
// point that makes the value visible as a workflow output.
package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	schemacontract "github.com/Sly1029/massive/conformance/schema"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/datastore"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	JSONContentType     = "application/json"
	ManifestContentType = "application/vnd.massive.data-artifact-manifest+json"
	manifestSchemaRef   = "https://massive.dev/conformance/schema/data-artifact-manifest.schema.json"
)

var (
	ErrValidation       = errors.New("artifact validation failed")
	ErrIntegrity        = errors.New("artifact integrity check failed")
	ErrBodyConflict     = errors.New("artifact body conflict")
	ErrManifestConflict = errors.New("artifact manifest conflict")
)

var (
	safePathSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9_.@:#-]+$`)
	safeProjectKeyPattern  = regexp.MustCompile(`^[A-Za-z0-9_.@:#-]+(/[A-Za-z0-9_.@:#-]+)*$`)
)

type Destination struct {
	ManifestKey datastore.Key
	Schema      string
}

type Producer struct {
	ProjectKey string `json:"projectKey"`
	PlanHash   string `json:"planHash"`
	RunID      string `json:"runId"`
	NodeID     string `json:"nodeId"`
	Attempt    int    `json:"attempt"`
}

type ArtifactRef struct {
	Key         string `json:"key"`
	Hash        string `json:"hash"`
	Size        int    `json:"size"`
	ContentType string `json:"contentType"`
}

type PublishedJSON struct {
	Manifest ArtifactRef
	Body     ArtifactRef
	Schema   string
}

type dataArtifactManifest struct {
	Kind          string      `json:"kind"`
	SchemaVersion uint32      `json:"schemaVersion"`
	Encoding      string      `json:"encoding"`
	Producer      Producer    `json:"producer"`
	Schema        string      `json:"schema"`
	Body          ArtifactRef `json:"body"`
}

// PublishJSON validates canonical JSON, convergently installs its content body,
// then conditionally creates the immutable manifest that commits visibility.
func PublishJSON(ctx context.Context, store datastore.Datastore, destination Destination, producer Producer, body []byte) (PublishedJSON, error) {
	if err := validateDestination(destination, producer); err != nil {
		return PublishedJSON{}, err
	}
	if err := validateCanonicalJSON(ctx, store, destination.Schema, body); err != nil {
		return PublishedJSON{}, err
	}

	bodyHash := canonical.DigestBytes(body)
	bodyKey, err := blobKey(bodyHash)
	if err != nil {
		return PublishedJSON{}, err
	}
	bodyRef := ArtifactRef{Key: bodyKey.String(), Hash: bodyHash, Size: len(body), ContentType: JSONContentType}
	manifest := dataArtifactManifest{
		Kind:          "DataArtifactManifest",
		SchemaVersion: 0,
		Encoding:      "canonical-json-v0",
		Producer:      producer,
		Schema:        destination.Schema,
		Body:          bodyRef,
	}
	manifestBytes, err := canonicalManifest(manifest)
	if err != nil {
		return PublishedJSON{}, err
	}

	if err := putImmutable(ctx, store, bodyKey, body, JSONContentType, ErrBodyConflict); err != nil {
		return PublishedJSON{}, err
	}
	if err := putImmutable(ctx, store, destination.ManifestKey, manifestBytes, ManifestContentType, ErrManifestConflict); err != nil {
		return PublishedJSON{}, err
	}

	return PublishedJSON{
		Manifest: ArtifactRef{
			Key:         destination.ManifestKey.String(),
			Hash:        canonical.DigestBytes(manifestBytes),
			Size:        len(manifestBytes),
			ContentType: ManifestContentType,
		},
		Body:   bodyRef,
		Schema: destination.Schema,
	}, nil
}

// ResolveJSON returns a value only after verifying its immutable manifest,
// producer, content-addressed body, canonical encoding, and pinned schema.
func ResolveJSON(ctx context.Context, store datastore.Datastore, destination Destination, producer Producer) (PublishedJSON, []byte, error) {
	if err := validateDestination(destination, producer); err != nil {
		return PublishedJSON{}, nil, err
	}
	manifestObject, err := store.Get(ctx, destination.ManifestKey)
	if err != nil {
		return PublishedJSON{}, nil, fmt.Errorf("read artifact manifest %s: %w", destination.ManifestKey, err)
	}
	if manifestObject.Info.ContentType != ManifestContentType {
		return PublishedJSON{}, nil, fmt.Errorf("%w: manifest %s content type is %q", ErrIntegrity, destination.ManifestKey, manifestObject.Info.ContentType)
	}
	canonicalBytes, err := canonical.CanonicalizeJSON(manifestObject.Body)
	if err != nil || !bytes.Equal(canonicalBytes, manifestObject.Body) {
		return PublishedJSON{}, nil, fmt.Errorf("%w: manifest %s is not canonical JSON", ErrIntegrity, destination.ManifestKey)
	}
	if err := validateManifestSchema(manifestObject.Body); err != nil {
		return PublishedJSON{}, nil, fmt.Errorf("%w: manifest %s: %v", ErrIntegrity, destination.ManifestKey, err)
	}

	var manifest dataArtifactManifest
	if err := json.Unmarshal(manifestObject.Body, &manifest); err != nil {
		return PublishedJSON{}, nil, fmt.Errorf("%w: decode manifest %s: %v", ErrIntegrity, destination.ManifestKey, err)
	}
	if manifest.Producer != producer || manifest.Schema != destination.Schema {
		return PublishedJSON{}, nil, fmt.Errorf("%w: manifest %s does not match its expected producer and schema", ErrIntegrity, destination.ManifestKey)
	}
	bodyKey, err := blobKey(manifest.Body.Hash)
	if err != nil || bodyKey.String() != manifest.Body.Key {
		return PublishedJSON{}, nil, fmt.Errorf("%w: manifest %s body key does not match its digest", ErrIntegrity, destination.ManifestKey)
	}
	bodyObject, err := store.Get(ctx, bodyKey)
	if err != nil {
		return PublishedJSON{}, nil, fmt.Errorf("%w: read manifest body %s: %v", ErrIntegrity, bodyKey, err)
	}
	if bodyObject.Info.ContentType != JSONContentType || len(bodyObject.Body) != manifest.Body.Size || canonical.DigestBytes(bodyObject.Body) != manifest.Body.Hash {
		return PublishedJSON{}, nil, fmt.Errorf("%w: body %s does not match its manifest", ErrIntegrity, bodyKey)
	}
	if err := validateCanonicalJSON(ctx, store, manifest.Schema, bodyObject.Body); err != nil {
		return PublishedJSON{}, nil, fmt.Errorf("%w: body %s: %v", ErrIntegrity, bodyKey, err)
	}

	return PublishedJSON{
		Manifest: ArtifactRef{Key: destination.ManifestKey.String(), Hash: canonical.DigestBytes(manifestObject.Body), Size: len(manifestObject.Body), ContentType: ManifestContentType},
		Body:     manifest.Body,
		Schema:   manifest.Schema,
	}, bodyObject.Body, nil
}

func validateDestination(destination Destination, producer Producer) error {
	if err := validateProducerIdentity(producer); err != nil {
		return err
	}
	want, err := datastore.ParseKey("projects/" + producer.ProjectKey + "/runs/" + producer.RunID + "/steps/" + producer.NodeID + "/" + strconv.Itoa(producer.Attempt) + "/output-manifest.json")
	if err != nil {
		return fmt.Errorf("%w: invalid producer identity: %v", ErrValidation, err)
	}
	if destination.ManifestKey != want {
		return fmt.Errorf("%w: manifest destination %s does not match producer slot %s", ErrValidation, destination.ManifestKey, want)
	}
	return nil
}

func validateProducerIdentity(producer Producer) error {
	if !isSafeProjectKey(producer.ProjectKey) {
		return fmt.Errorf("%w: project key %q is not a safe relative path", ErrValidation, producer.ProjectKey)
	}
	if !isSafePathSegment(producer.RunID) {
		return fmt.Errorf("%w: run ID %q is not a safe path segment", ErrValidation, producer.RunID)
	}
	if !isSafePathSegment(producer.NodeID) {
		return fmt.Errorf("%w: node ID %q is not a safe path segment", ErrValidation, producer.NodeID)
	}
	if producer.Attempt < 1 {
		return fmt.Errorf("%w: attempt must be at least one", ErrValidation)
	}
	return nil
}

func isSafePathSegment(value string) bool {
	return safePathSegmentPattern.MatchString(value) && value != "." && value != ".."
}

func isSafeProjectKey(value string) bool {
	if !safeProjectKeyPattern.MatchString(value) {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if !isSafePathSegment(segment) {
			return false
		}
	}
	return true
}

func validateCanonicalJSON(ctx context.Context, store datastore.Datastore, schemaRef string, body []byte) error {
	canonicalBody, err := canonical.CanonicalizeJSON(body)
	if err != nil || !bytes.Equal(canonicalBody, body) {
		return fmt.Errorf("%w: value is not canonical JSON", ErrValidation)
	}
	schemaKey, err := blobKey(schemaRef)
	if err != nil {
		return fmt.Errorf("%w: invalid schema reference %q", ErrValidation, schemaRef)
	}
	schemaObject, err := store.Get(ctx, schemaKey)
	if err != nil {
		return fmt.Errorf("%w: read schema %s: %v", ErrValidation, schemaKey, err)
	}
	canonicalSchema, err := canonical.CanonicalizeJSON(schemaObject.Body)
	if err != nil || !bytes.Equal(canonicalSchema, schemaObject.Body) || canonical.DigestBytes(schemaObject.Body) != schemaRef {
		return fmt.Errorf("%w: schema %s is not canonical or does not match its digest", ErrValidation, schemaRef)
	}
	if err := validateJSONSchema(schemaObject.Body, body); err != nil {
		return fmt.Errorf("%w: value does not satisfy schema %s: %v", ErrValidation, schemaRef, err)
	}
	return nil
}

func putImmutable(ctx context.Context, store datastore.Datastore, key datastore.Key, body []byte, contentType string, conflict error) error {
	_, err := store.Put(ctx, key, body, datastore.PutOptions{ContentType: contentType, IfAbsent: true})
	if err == nil {
		return nil
	}
	if !errors.Is(err, datastore.ErrAlreadyExists) {
		return fmt.Errorf("publish immutable object %s: %w", key, err)
	}
	existing, getErr := store.Get(ctx, key)
	if getErr != nil {
		return fmt.Errorf("%w: read existing object %s after create conflict: %v", conflict, key, getErr)
	}
	if existing.Info.ContentType != contentType || !bytes.Equal(existing.Body, body) {
		return fmt.Errorf("%w: existing object %s differs", conflict, key)
	}
	return nil
}

func canonicalManifest(manifest dataArtifactManifest) ([]byte, error) {
	raw, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("marshal artifact manifest: %w", err)
	}
	encoded, err := canonical.CanonicalizeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize artifact manifest: %w", err)
	}
	if err := validateManifestSchema(encoded); err != nil {
		return nil, fmt.Errorf("%w: artifact manifest: %v", ErrValidation, err)
	}
	return encoded, nil
}

func blobKey(hash string) (datastore.Key, error) {
	digest, ok := strings.CutPrefix(hash, "sha256:")
	if !ok {
		return datastore.Key{}, fmt.Errorf("invalid SHA-256 reference %q", hash)
	}
	return datastore.BlobKeySHA256Hex(digest)
}

func validateJSONSchema(schemaBytes, documentBytes []byte) error {
	schemaDocument, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaBytes))
	if err != nil {
		return fmt.Errorf("decode schema: %w", err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(documentBytes))
	if err != nil {
		return fmt.Errorf("decode document: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("schema.json", schemaDocument); err != nil {
		return fmt.Errorf("register schema: %w", err)
	}
	compiled, err := compiler.Compile("schema.json")
	if err != nil {
		return fmt.Errorf("compile schema: %w", err)
	}
	return compiled.Validate(instance)
}

var manifestSchemaOnce sync.Once
var manifestSchema *jsonschema.Schema
var manifestSchemaErr error

func validateManifestSchema(document []byte) error {
	manifestSchemaOnce.Do(func() {
		schemaDocument, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemacontract.DataArtifactManifestSchemaJSON))
		if err != nil {
			manifestSchemaErr = err
			return
		}
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource(manifestSchemaRef, schemaDocument); err != nil {
			manifestSchemaErr = err
			return
		}
		manifestSchema, manifestSchemaErr = compiler.Compile(manifestSchemaRef)
	})
	if manifestSchemaErr != nil {
		return manifestSchemaErr
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(document))
	if err != nil {
		return err
	}
	return manifestSchema.Validate(instance)
}
