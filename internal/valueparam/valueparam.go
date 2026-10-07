// Package valueparam carries canonical JSON values through target scheduler
// parameters, such as Argo task inputs and outputs. Small values travel inline;
// larger values travel as a reference to their content-addressed body in the
// shared datastore. A reference never changes a value's identity: its hash is
// the SHA-256 of the same canonical bytes that artifact manifests record.
//
// A parameter is either canonical JSON text or "@" followed by the canonical
// JSON of a Ref. "@" cannot begin JSON text, so the two forms are unambiguous
// for every value. Indexed map envelopes carry the same Ref in a "ref" field.
package valueparam

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	schemacontract "github.com/Sly1029/massive/conformance/schema"
	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/mapexec"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	// InlineLimit bounds a value passed alone. Argo copies parameters into pod
	// arguments and its template environment variable, which Linux limits to
	// 128 KiB per string, and merges several parameters into one pod.
	InlineLimit = 4096
	// ItemInlineLimit bounds a value inside an indexed map envelope. Argo
	// concatenates every item result into the collector's single parameter,
	// so items must stay small for wide maps to fit.
	ItemInlineLimit = 256
	// ContentType is the media type of referenced bodies.
	ContentType = "application/json"

	referencePrefix = '@'
)

// Ref identifies one canonical JSON body stored at blobs/sha256/<hex>.
type Ref struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

// Codec resolves and publishes referenced bodies in one datastore.
type Codec struct {
	Store datastore.Datastore
}

// Value is a decoded parameter. Ref is set when the parameter was a reference,
// so a control task can forward it without republishing the body.
type Value struct {
	Body []byte
	Ref  *Ref
}

// Parameter returns the parameter text that forwards this value unchanged.
func (value Value) Parameter() []byte {
	if value.Ref != nil {
		return refParameter(*value.Ref)
	}
	return value.Body
}

// Encode returns canonical JSON inline when it fits InlineLimit; otherwise it
// publishes the body and returns its reference.
func (codec Codec) Encode(ctx context.Context, body []byte) ([]byte, error) {
	body, err := canonicalBody(body)
	if err != nil {
		return nil, err
	}
	if len(body) <= InlineLimit {
		return body, nil
	}
	ref, err := codec.publish(ctx, body)
	if err != nil {
		return nil, err
	}
	return refParameter(ref), nil
}

// EncodePublished is Encode for a step output the language runner has
// already committed at its content-addressed key, so no upload is needed.
func EncodePublished(body []byte) ([]byte, error) {
	body, err := canonicalBody(body)
	if err != nil {
		return nil, err
	}
	if len(body) <= InlineLimit {
		return body, nil
	}
	return refParameter(refFor(body)), nil
}

// Decode resolves inline JSON or a reference to verified canonical JSON.
func (codec Codec) Decode(ctx context.Context, parameter []byte) (Value, error) {
	parameter = bytes.TrimSpace(parameter)
	if len(parameter) > 0 && parameter[0] == referencePrefix {
		ref, err := parseRef(parameter[1:])
		if err != nil {
			return Value{}, err
		}
		body, err := codec.resolve(ctx, ref)
		if err != nil {
			return Value{}, err
		}
		return Value{Body: body, Ref: &ref}, nil
	}
	body, err := canonical.CanonicalizeJSON(parameter)
	if err != nil {
		return Value{}, fmt.Errorf("value parameter is neither JSON nor a reference: %w", err)
	}
	return Value{Body: body}, nil
}

type envelope struct {
	Index *int            `json:"index,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
	Ref   json.RawMessage `json:"ref,omitempty"`
	Empty bool            `json:"empty,omitempty"`
}

// ExpandItems projects a map input parameter into indexed loop envelopes.
// Argo does not expose a stable source index for withParam items, so the
// index travels with each value. A singleton empty marker keeps a zero-item
// loop's aggregate output resolvable; it invokes no user code.
func (codec Codec) ExpandItems(ctx context.Context, parameter []byte) ([]byte, error) {
	input, err := codec.Decode(ctx, parameter)
	if err != nil {
		return nil, err
	}
	items, err := mapexec.Expand(input.Body)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return canonical.Marshal([]map[string]bool{{"empty": true}})
	}
	envelopes := make([]json.RawMessage, len(items))
	for position, item := range items {
		fields := map[string]any{"index": item.Index, "value": json.RawMessage(item.Body)}
		if len(item.Body) > ItemInlineLimit {
			ref, err := codec.publish(ctx, item.Body)
			if err != nil {
				return nil, err
			}
			fields = map[string]any{"index": item.Index, "ref": ref}
		}
		if envelopes[position], err = canonical.Marshal(fields); err != nil {
			return nil, err
		}
	}
	return canonical.Marshal(envelopes)
}

// DecodeItem resolves one indexed loop envelope or reports the empty marker.
func (codec Codec) DecodeItem(ctx context.Context, body []byte) (mapexec.Item, bool, error) {
	index, value, empty, err := codec.decodeEnvelope(ctx, body)
	if err != nil {
		return mapexec.Item{}, false, fmt.Errorf("parse Argo map item: %w", err)
	}
	return mapexec.Item{Index: index, Body: value}, empty, nil
}

// EncodePublishedItemResult binds a committed mapper result to its source index.
func EncodePublishedItemResult(index int, body []byte) ([]byte, error) {
	if index < 0 {
		return nil, fmt.Errorf("map result index %d must be nonnegative", index)
	}
	body, err := canonicalBody(body)
	if err != nil {
		return nil, fmt.Errorf("map item %d output: %w", index, err)
	}
	if len(body) > ItemInlineLimit {
		return canonical.Marshal(map[string]any{"index": index, "ref": refFor(body)})
	}
	return canonical.Marshal(map[string]any{"index": index, "value": json.RawMessage(body)})
}

// CollectResults unwraps Argo's aggregate output, accepting both raw JSON
// objects and the JSON-string form used by some Argo releases, resolves each
// item, and returns the source-ordered list as a parameter.
func (codec Codec) CollectResults(ctx context.Context, aggregate []byte) ([]byte, error) {
	var values []json.RawMessage
	if err := json.Unmarshal(aggregate, &values); err != nil {
		return nil, fmt.Errorf("decode Argo map results: %w", err)
	}
	results := make([]mapexec.Result, 0, len(values))
	emptyMarkers := 0
	for position, value := range values {
		body := []byte(value)
		if len(body) > 0 && body[0] == '"' {
			var encoded string
			if err := json.Unmarshal(body, &encoded); err != nil {
				return nil, fmt.Errorf("decode Argo map result %d string: %w", position, err)
			}
			body = []byte(encoded)
		}
		index, result, empty, err := codec.decodeEnvelope(ctx, body)
		if err != nil {
			return nil, fmt.Errorf("parse Argo map result %d: %w", position, err)
		}
		if empty {
			emptyMarkers++
			continue
		}
		results = append(results, mapexec.Result{Index: index, Body: result})
	}
	if emptyMarkers > 0 {
		if emptyMarkers != 1 || len(values) != 1 {
			return nil, errors.New("empty-map marker cannot be combined with map results")
		}
		return []byte(`[]`), nil
	}
	collected, err := mapexec.Collect(len(results), results)
	if err != nil {
		return nil, err
	}
	return codec.Encode(ctx, collected)
}

func (codec Codec) decodeEnvelope(ctx context.Context, body []byte) (int, []byte, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var fields envelope
	if err := decoder.Decode(&fields); err != nil {
		return 0, nil, false, fmt.Errorf("decode map envelope: %w", err)
	}
	if fields.Empty {
		if fields.Index != nil || fields.Value != nil || fields.Ref != nil {
			return 0, nil, false, errors.New("empty-map marker must contain only empty")
		}
		return 0, nil, true, nil
	}
	if fields.Index == nil || *fields.Index < 0 || (fields.Value == nil) == (fields.Ref == nil) {
		return 0, nil, false, errors.New("map envelope must contain a nonnegative index and exactly one of value or ref")
	}
	if fields.Ref != nil {
		ref, err := parseRef(fields.Ref)
		if err != nil {
			return 0, nil, false, err
		}
		value, err := codec.resolve(ctx, ref)
		return *fields.Index, value, false, err
	}
	value, err := canonical.CanonicalizeJSON(fields.Value)
	if err != nil {
		return 0, nil, false, fmt.Errorf("canonicalize map envelope value: %w", err)
	}
	return *fields.Index, value, false, nil
}

func (codec Codec) publish(ctx context.Context, body []byte) (Ref, error) {
	if codec.Store == nil {
		return Ref{}, errors.New("a value above the inline limit requires the shared datastore")
	}
	ref := refFor(body)
	key, err := blobKey(ref.Hash)
	if err != nil {
		return Ref{}, err
	}
	// The key is the body's digest, so an existing object is the same value;
	// every reader verifies digest, size, and content type before use.
	if _, err := codec.Store.Put(ctx, key, body, datastore.PutOptions{ContentType: ContentType, IfAbsent: true}); err != nil && !errors.Is(err, datastore.ErrAlreadyExists) {
		return Ref{}, fmt.Errorf("publish referenced value %s: %w", ref.Hash, err)
	}
	return ref, nil
}

func (codec Codec) resolve(ctx context.Context, ref Ref) ([]byte, error) {
	if codec.Store == nil {
		return nil, errors.New("a referenced value requires the shared datastore")
	}
	key, err := blobKey(ref.Hash)
	if err != nil {
		return nil, err
	}
	object, err := codec.Store.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("read referenced value %s: %w", ref.Hash, err)
	}
	if object.Info.ContentType != ContentType || int64(len(object.Body)) != ref.Size || canonical.DigestBytes(object.Body) != ref.Hash {
		return nil, fmt.Errorf("referenced value %s does not match its hash, size, or content type", ref.Hash)
	}
	body, err := canonicalBody(object.Body)
	if err != nil {
		return nil, fmt.Errorf("referenced value %s: %w", ref.Hash, err)
	}
	return body, nil
}

func canonicalBody(body []byte) ([]byte, error) {
	canonicalized, err := canonical.CanonicalizeJSON(body)
	if err != nil || !bytes.Equal(canonicalized, body) {
		return nil, errors.New("value must be canonical JSON")
	}
	return body, nil
}

func refFor(body []byte) Ref {
	return Ref{Hash: canonical.DigestBytes(body), Size: int64(len(body))}
}

func refParameter(ref Ref) []byte {
	body, err := canonical.Marshal(ref)
	if err != nil {
		panic(err) // Ref contains only a string and an integer.
	}
	return append([]byte{referencePrefix}, body...)
}

func parseRef(body []byte) (Ref, error) {
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		return Ref{}, fmt.Errorf("decode value reference: %w", err)
	}
	schema, err := refSchema()
	if err != nil {
		return Ref{}, err
	}
	if err := schema.Validate(instance); err != nil {
		return Ref{}, fmt.Errorf("invalid value reference: %w", err)
	}
	var ref Ref
	if err := json.Unmarshal(body, &ref); err != nil {
		return Ref{}, err
	}
	return ref, nil
}

var refSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemacontract.ValueReferenceSchemaJSON))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("value-reference.schema.json", document); err != nil {
		return nil, err
	}
	return compiler.Compile("value-reference.schema.json")
})

func blobKey(hash string) (datastore.Key, error) {
	return datastore.BlobKeySHA256Hex(strings.TrimPrefix(hash, "sha256:"))
}
