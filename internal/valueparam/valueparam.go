// Package valueparam carries canonical JSON values through target scheduler
// parameters, such as Argo task inputs and outputs. Small values travel inline;
// larger values travel as a reference to their content-addressed body in the
// shared datastore. A reference never changes a value's identity: its hash is
// the SHA-256 of the same canonical bytes that artifact manifests record.
//
// Between tasks a parameter has exactly one valid spelling: canonical JSON of
// at most InlineLimit bytes, or "@" followed by the canonical JSON of a Ref to
// a larger body. "@" cannot begin JSON text, so the forms are unambiguous for
// every value. Workflow inputs are the one exception: submitters write
// ordinary inline JSON, which is normalized once at the workflow entry.
package valueparam

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
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
	// ItemInlineLimit bounds a mapper result inside an indexed envelope. A
	// reference envelope costs about 120 bytes, so inlining anything larger
	// would only make the collector's aggregated parameter bigger.
	ItemInlineLimit = 100
	// MaxValueBytes bounds any referenced body a runtime pod will read.
	MaxValueBytes = 256 * 1024 * 1024
	// ContentType is the media type of referenced bodies.
	ContentType = "application/json"

	referencePrefix = '@'
	// maxEnvelopeBytes bounds one item result envelope: an index of at most
	// MaxMapItems plus an inline value of ItemInlineLimit bytes or a Ref.
	maxEnvelopeBytes = 128
	// argoParameterBudget leaves headroom below Linux's 128 KiB per string for
	// the rest of a collector's Argo template.
	argoParameterBudget = 96 * 1024
	// MaxMapItems is the widest map whose collected envelopes fit Argo: the
	// aggregate appears twice in the JSON-escaped template environment, and
	// escaping adds at most 16 bytes to an envelope.
	MaxMapItems = argoParameterBudget / (2 * (maxEnvelopeBytes + 16))
)

// ErrContract marks a parameter that violates this encoding. Runtimes report
// it as a non-retryable descriptor failure.
var ErrContract = errors.New("value parameter contract violation")

func contractError(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrContract}, args...)...)
}

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
// so a control task can forward it without reading or publishing again.
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

// DecodeEntry normalizes a submitted workflow input: any inline JSON is
// canonicalized; references are rejected, because a submitter must not point
// pods at arbitrary bodies in the shared store.
func DecodeEntry(parameter []byte) (Value, error) {
	if len(parameter) > 0 && parameter[0] == referencePrefix {
		return Value{}, contractError("workflow inputs must be inline JSON, not value references")
	}
	body, err := canonical.CanonicalizeJSON(parameter)
	if err != nil {
		return Value{}, contractError("workflow input is not JSON: %v", err)
	}
	return Value{Body: body}, nil
}

// Encode returns canonical JSON inline when it fits InlineLimit; otherwise it
// publishes the body and returns its reference.
func (codec Codec) Encode(ctx context.Context, body []byte) ([]byte, error) {
	if err := requireCanonical(body); err != nil {
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
	if err := requireCanonical(body); err != nil {
		return nil, err
	}
	if len(body) <= InlineLimit {
		return body, nil
	}
	return refParameter(refFor(body)), nil
}

// Decode accepts only the exact spelling Encode produces for a value.
func (codec Codec) Decode(ctx context.Context, parameter []byte) (Value, error) {
	if len(parameter) > 0 && parameter[0] == referencePrefix {
		ref, err := parseRef(parameter[1:], InlineLimit)
		if err != nil {
			return Value{}, err
		}
		body, err := codec.resolve(ctx, ref)
		if err != nil {
			return Value{}, err
		}
		return Value{Body: body, Ref: &ref}, nil
	}
	if err := requireCanonical(parameter); err != nil {
		return Value{}, err
	}
	if len(parameter) > InlineLimit {
		return Value{}, contractError("an inline value of %d bytes must be a reference", len(parameter))
	}
	return Value{Body: parameter}, nil
}

// DecodeMerge assembles a merge step's ordered array input from one parameter
// per source. It sums the sources' declared sizes before reading any
// referenced body, so k sources cannot build an input above MaxValueBytes.
func (codec Codec) DecodeMerge(ctx context.Context, parameters []string) ([]byte, error) {
	// The brackets and separating commas add one byte per source plus one.
	total := int64(len(parameters) + 1)
	for index, parameter := range parameters {
		size := int64(len(parameter))
		if len(parameter) > 0 && parameter[0] == referencePrefix {
			ref, err := parseRef([]byte(parameter[1:]), InlineLimit)
			if err != nil {
				return nil, fmt.Errorf("merge input %d: %w", index, err)
			}
			size = ref.Size
		}
		total += size
	}
	if total > MaxValueBytes {
		return nil, contractError("merging %d sources would build a %d-byte input, above the %d-byte value limit", len(parameters), total, MaxValueBytes)
	}
	bodies := make([][]byte, len(parameters))
	for index, parameter := range parameters {
		value, err := codec.Decode(ctx, []byte(parameter))
		if err != nil {
			return nil, fmt.Errorf("merge input %d: %w", index, err)
		}
		bodies[index] = value.Body
	}
	return slices.Concat([]byte("["), bytes.Join(bodies, []byte(",")), []byte("]")), nil
}

// envelope is one Argo loop item or mapper result. Argo re-serializes these
// JSON objects when it substitutes {{item}} and aggregates outputs, so
// envelopes are canonicalized on receipt; their values and refs are checked.
type envelope struct {
	Index   *int            `json:"index,omitempty"`
	Value   json.RawMessage `json:"value,omitempty"`
	Ref     json.RawMessage `json:"ref,omitempty"`
	ListRef json.RawMessage `json:"listRef,omitempty"`
	Empty   bool            `json:"empty,omitempty"`
}

// ExpandItems projects a map input parameter into indexed loop envelopes
// without writing to the store. Items of an inline list travel inline; items
// of a referenced list carry that list's reference, and each item pod reads
// the list and selects its own index. A singleton empty marker keeps a
// zero-item loop's aggregate output resolvable; it invokes no user code.
func (codec Codec) ExpandItems(ctx context.Context, parameter []byte) ([]byte, error) {
	input, err := codec.Decode(ctx, parameter)
	if err != nil {
		return nil, err
	}
	items, err := mapexec.Expand(input.Body)
	if err != nil {
		return nil, err
	}
	if len(items) > MaxMapItems {
		return nil, contractError("map has %d items, but Argo can collect at most %d item results in one parameter; split the work into fewer, larger items", len(items), MaxMapItems)
	}
	if len(items) == 0 {
		return canonical.Marshal([]map[string]bool{{"empty": true}})
	}
	envelopes := make([]json.RawMessage, len(items))
	for position, item := range items {
		fields := map[string]any{"index": item.Index, "value": json.RawMessage(item.Body)}
		if input.Ref != nil {
			fields = map[string]any{"index": item.Index, "listRef": *input.Ref}
		}
		if envelopes[position], err = canonical.Marshal(fields); err != nil {
			return nil, err
		}
	}
	return canonical.Marshal(envelopes)
}

// DecodeItem resolves one indexed loop envelope or reports the empty marker.
func (codec Codec) DecodeItem(ctx context.Context, body []byte) (mapexec.Item, bool, error) {
	fields, err := parseEnvelope(body)
	if err != nil {
		return mapexec.Item{}, false, err
	}
	if fields.Empty {
		return mapexec.Item{}, true, nil
	}
	if fields.Ref != nil || (fields.Value == nil) == (fields.ListRef == nil) {
		return mapexec.Item{}, false, contractError("map item must contain an index and exactly one of value or listRef")
	}
	if fields.Value != nil {
		value, err := canonical.CanonicalizeJSON(fields.Value)
		if err != nil {
			return mapexec.Item{}, false, contractError("map item value: %v", err)
		}
		return mapexec.Item{Index: *fields.Index, Body: value}, false, nil
	}
	ref, err := parseRef(fields.ListRef, InlineLimit)
	if err != nil {
		return mapexec.Item{}, false, err
	}
	list, err := codec.resolve(ctx, ref)
	if err != nil {
		return mapexec.Item{}, false, err
	}
	items, err := mapexec.Expand(list)
	if err != nil {
		return mapexec.Item{}, false, err
	}
	if *fields.Index >= len(items) {
		return mapexec.Item{}, false, contractError("map item index %d is outside its %d-item list", *fields.Index, len(items))
	}
	return items[*fields.Index], false, nil
}

// EncodePublishedItemResult binds a committed mapper result to its source index.
func EncodePublishedItemResult(index int, body []byte) ([]byte, error) {
	if index < 0 {
		return nil, fmt.Errorf("map result index %d must be nonnegative", index)
	}
	if err := requireCanonical(body); err != nil {
		return nil, fmt.Errorf("map item %d output: %w", index, err)
	}
	if len(body) > ItemInlineLimit {
		return canonical.Marshal(map[string]any{"index": index, "ref": refFor(body)})
	}
	return canonical.Marshal(map[string]any{"index": index, "value": json.RawMessage(body)})
}

// CollectResults unwraps Argo's aggregate output, accepting both raw JSON
// objects and the JSON-string form used by some Argo releases, resolves each
// item, and returns the source-ordered list as a parameter. It is the only
// control operation that publishes a body.
func (codec Codec) CollectResults(ctx context.Context, aggregate []byte) ([]byte, error) {
	var values []json.RawMessage
	if err := json.Unmarshal(aggregate, &values); err != nil {
		return nil, contractError("decode Argo map results: %v", err)
	}
	results := make([]mapexec.Result, 0, len(values))
	emptyMarkers := 0
	for position, value := range values {
		body := []byte(value)
		if len(body) > 0 && body[0] == '"' {
			var encoded string
			if err := json.Unmarshal(body, &encoded); err != nil {
				return nil, contractError("decode Argo map result %d string: %v", position, err)
			}
			body = []byte(encoded)
		}
		fields, err := parseEnvelope(body)
		if err != nil {
			return nil, fmt.Errorf("map result %d: %w", position, err)
		}
		if fields.Empty {
			emptyMarkers++
			continue
		}
		if fields.ListRef != nil || (fields.Value == nil) == (fields.Ref == nil) {
			return nil, contractError("map result %d must contain an index and exactly one of value or ref", position)
		}
		var result []byte
		if fields.Value != nil {
			if result, err = canonical.CanonicalizeJSON(fields.Value); err != nil || len(result) > ItemInlineLimit {
				return nil, contractError("map result %d must be canonical JSON of at most %d bytes inline", position, ItemInlineLimit)
			}
		} else {
			ref, err := parseRef(fields.Ref, ItemInlineLimit)
			if err != nil {
				return nil, fmt.Errorf("map result %d: %w", position, err)
			}
			if result, err = codec.resolve(ctx, ref); err != nil {
				return nil, err
			}
		}
		results = append(results, mapexec.Result{Index: *fields.Index, Body: result})
	}
	if emptyMarkers > 0 {
		if emptyMarkers != 1 || len(values) != 1 {
			return nil, contractError("empty-map marker cannot be combined with map results")
		}
		return []byte(`[]`), nil
	}
	collected, err := mapexec.Collect(len(results), results)
	if err != nil {
		return nil, err
	}
	return codec.Encode(ctx, collected)
}

func parseEnvelope(body []byte) (envelope, error) {
	canonicalBody, err := canonical.CanonicalizeJSON(body)
	if err != nil {
		return envelope{}, contractError("map envelope must be JSON: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(canonicalBody))
	decoder.DisallowUnknownFields()
	var fields envelope
	if err := decoder.Decode(&fields); err != nil {
		return envelope{}, contractError("decode map envelope: %v", err)
	}
	if fields.Empty {
		if fields.Index != nil || fields.Value != nil || fields.Ref != nil || fields.ListRef != nil {
			return envelope{}, contractError("empty-map marker must contain only empty")
		}
		return fields, nil
	}
	if fields.Index == nil || *fields.Index < 0 {
		return envelope{}, contractError("map envelope needs a nonnegative index")
	}
	return fields, nil
}

func (codec Codec) publish(ctx context.Context, body []byte) (Ref, error) {
	if codec.Store == nil {
		return Ref{}, errors.New("a value above the inline limit requires the shared datastore")
	}
	if len(body) > MaxValueBytes {
		return Ref{}, contractError("value is %d bytes, above the %d-byte limit", len(body), MaxValueBytes)
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

// resolve reads a referenced body only after its declared length matches the
// reference, and never reads past that length.
func (codec Codec) resolve(ctx context.Context, ref Ref) ([]byte, error) {
	if codec.Store == nil {
		return nil, errors.New("a referenced value requires the shared datastore")
	}
	key, err := blobKey(ref.Hash)
	if err != nil {
		return nil, err
	}
	reader, info, err := codec.Store.Open(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("read referenced value %s: %w", ref.Hash, err)
	}
	defer reader.Close()
	if info.ContentType != ContentType || info.Size != ref.Size {
		return nil, contractError("referenced value %s is %d bytes of %s, not %d bytes of %s", ref.Hash, info.Size, info.ContentType, ref.Size, ContentType)
	}
	body, err := io.ReadAll(io.LimitReader(reader, ref.Size+1))
	if err != nil {
		return nil, fmt.Errorf("read referenced value %s: %w", ref.Hash, err)
	}
	if int64(len(body)) != ref.Size || canonical.DigestBytes(body) != ref.Hash {
		return nil, contractError("referenced value %s does not match its hash or size", ref.Hash)
	}
	if err := requireCanonical(body); err != nil {
		return nil, fmt.Errorf("referenced value %s: %w", ref.Hash, err)
	}
	return body, nil
}

func requireCanonical(body []byte) error {
	canonicalized, err := canonical.CanonicalizeJSON(body)
	if err != nil || !bytes.Equal(canonicalized, body) {
		return contractError("value must be canonical JSON")
	}
	return nil
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

// parseRef accepts only the canonical, schema-valid spelling of a reference to
// a body larger than inlineLimit: a smaller body has an inline spelling.
func parseRef(body []byte, inlineLimit int) (Ref, error) {
	if err := requireCanonical(body); err != nil {
		return Ref{}, contractError("value reference must be canonical JSON")
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		return Ref{}, contractError("decode value reference: %v", err)
	}
	schema, err := refSchema()
	if err != nil {
		return Ref{}, err
	}
	if err := schema.Validate(instance); err != nil {
		return Ref{}, contractError("invalid value reference: %v", err)
	}
	var ref Ref
	if err := json.Unmarshal(body, &ref); err != nil {
		return Ref{}, err
	}
	if ref.Size <= int64(inlineLimit) {
		return Ref{}, contractError("a %d-byte value must be inline, not a reference", ref.Size)
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
