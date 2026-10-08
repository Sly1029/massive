package valueparam_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sly1029/massive/internal/canonical"
	"github.com/Sly1029/massive/internal/datastore"
	"github.com/Sly1029/massive/internal/valueparam"
)

type vectors struct {
	InlineLimit     int `json:"inlineLimit"`
	ItemInlineLimit int `json:"itemInlineLimit"`
	MaxValueBytes   int `json:"maxValueBytes"`
	Cases           []struct {
		Name      string          `json:"name"`
		Kind      string          `json:"kind"`
		Input     string          `json:"input"`
		Value     json.RawMessage `json:"value"`
		Index     int             `json:"index"`
		Parameter string          `json:"parameter"`
		Envelope  string          `json:"envelope"`
	} `json:"cases"`
	Invalid []struct {
		Name      string `json:"name"`
		Context   string `json:"context"`
		Parameter string `json:"parameter"`
	} `json:"invalid"`
}

func newCodec(t *testing.T) (valueparam.Codec, datastore.Datastore) {
	t.Helper()
	store, err := datastore.NewLocalDatastore(datastore.LocalConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return valueparam.Codec{Store: store}, store
}

func TestConformanceVectors(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "conformance", "fixtures", "value-parameters", "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture vectors
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.InlineLimit != valueparam.InlineLimit || fixture.ItemInlineLimit != valueparam.ItemInlineLimit || fixture.MaxValueBytes != valueparam.MaxValueBytes {
		t.Fatalf("vector limits %d/%d/%d differ from implementation", fixture.InlineLimit, fixture.ItemInlineLimit, fixture.MaxValueBytes)
	}
	for _, vector := range fixture.Cases {
		t.Run(vector.Name, func(t *testing.T) {
			codec, _ := newCodec(t)
			body, err := canonical.CanonicalizeJSON(vector.Value)
			if err != nil {
				t.Fatal(err)
			}
			switch vector.Kind {
			case "standalone":
				parameter, err := codec.Encode(context.Background(), body)
				if err != nil {
					t.Fatal(err)
				}
				if string(parameter) != vector.Parameter {
					t.Fatalf("parameter = %.120s, want %.120s", parameter, vector.Parameter)
				}
				published, err := valueparam.EncodePublished(body)
				if err != nil || !bytes.Equal(published, parameter) {
					t.Fatalf("published encoding = %.120s, %v", published, err)
				}
				resolved, err := codec.Decode(context.Background(), parameter)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(resolved.Body, body) || !bytes.Equal(resolved.Parameter(), parameter) {
					t.Fatal("decoded value or forwarded parameter changed")
				}
			case "workflow-entry":
				entry, err := valueparam.DecodeEntry([]byte(vector.Input))
				if err != nil || !bytes.Equal(entry.Body, body) || entry.Ref != nil {
					t.Fatalf("entry = %.80s, %v", entry.Body, err)
				}
			case "item-result":
				envelope, err := valueparam.EncodePublishedItemResult(vector.Index, body)
				if err != nil {
					t.Fatal(err)
				}
				if string(envelope) != vector.Envelope {
					t.Fatalf("envelope = %s, want %s", envelope, vector.Envelope)
				}
			default:
				t.Fatalf("unknown vector kind %q", vector.Kind)
			}
		})
	}
	for _, vector := range fixture.Invalid {
		t.Run(vector.Name, func(t *testing.T) {
			codec, _ := newCodec(t)
			var err error
			switch vector.Context {
			case "standalone":
				_, err = codec.Decode(context.Background(), []byte(vector.Parameter))
			case "workflow-entry":
				_, err = valueparam.DecodeEntry([]byte(vector.Parameter))
			default:
				t.Fatalf("unknown context %q", vector.Context)
			}
			if !errors.Is(err, valueparam.ErrContract) {
				t.Fatalf("error = %v, want a contract violation", err)
			}
		})
	}
}

func TestReferencesResolveOnlyVerifiedBoundedBodies(t *testing.T) {
	codec, store := newCodec(t)
	large := []byte(`"` + strings.Repeat("x", valueparam.InlineLimit) + `"`)
	parameter, err := codec.Encode(context.Background(), large)
	if err != nil {
		t.Fatal(err)
	}
	if parameter[0] != '@' {
		t.Fatalf("large value was inlined: %.40s", parameter)
	}
	key := datastore.BlobKeyForBytes(large)
	object, err := store.Get(context.Background(), key)
	if err != nil || !bytes.Equal(object.Body, large) || object.Info.ContentType != valueparam.ContentType {
		t.Fatalf("published body = %v, %v", object.Info, err)
	}

	for name, planted := range map[string][]byte{
		"different bytes": append([]byte(`"`), append(bytes.Repeat([]byte("y"), valueparam.InlineLimit), '"')...),
		// A larger planted object is refused from its declared size.
		"larger object": append([]byte(`"`), append(bytes.Repeat([]byte("x"), 4*valueparam.InlineLimit), '"')...),
	} {
		t.Run(name, func(t *testing.T) {
			tampered, store := newCodec(t)
			if _, err := store.Put(context.Background(), key, planted, datastore.PutOptions{ContentType: valueparam.ContentType}); err != nil {
				t.Fatal(err)
			}
			if _, err := tampered.Decode(context.Background(), parameter); !errors.Is(err, valueparam.ErrContract) {
				t.Fatalf("tampered reference error = %v", err)
			}
		})
	}
	missing, _ := newCodec(t)
	if _, err := missing.Decode(context.Background(), parameter); err == nil {
		t.Fatal("missing referenced body was accepted")
	}
	if _, err := (valueparam.Codec{}).Encode(context.Background(), large); err == nil {
		t.Fatal("a large value cannot be referenced without a datastore")
	}
	if inline, err := (valueparam.Codec{}).Encode(context.Background(), []byte(`{"small":true}`)); err != nil || string(inline) != `{"small":true}` {
		t.Fatalf("small value = %s, %v", inline, err)
	}
}

func TestExpandIsReadOnlyAndItemsSliceReferencedLists(t *testing.T) {
	codec, store := newCodec(t)
	inline, err := codec.ExpandItems(context.Background(), []byte(`[3,3,{"name":"x"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(inline), `[{"index":0,"value":3},{"index":1,"value":3},{"index":2,"value":{"name":"x"}}]`; got != want {
		t.Fatalf("inline items = %s, want %s", got, want)
	}

	values := make([]string, 40)
	for index := range values {
		values[index] = fmt.Sprintf(`"%03d%s"`, index, strings.Repeat("z", 200))
	}
	list := []byte("[" + strings.Join(values, ",") + "]")
	parameter, err := codec.Encode(context.Background(), list)
	if err != nil {
		t.Fatal(err)
	}
	// Expansion must not write: an empty store holding only the list proves it.
	before, err := store.List(context.Background(), datastore.MustKey("blobs"))
	if err != nil {
		t.Fatal(err)
	}
	expanded, err := codec.ExpandItems(context.Background(), parameter)
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.List(context.Background(), datastore.MustKey("blobs"))
	if err != nil || len(after) != len(before) {
		t.Fatalf("expansion wrote to the store: %d objects before, %d after, %v", len(before), len(after), err)
	}
	var envelopes []json.RawMessage
	if err := json.Unmarshal(expanded, &envelopes); err != nil || len(envelopes) != len(values) {
		t.Fatalf("expanded = %.200s, %v", expanded, err)
	}
	results := make([]json.RawMessage, 0, len(envelopes))
	for position := len(envelopes) - 1; position >= 0; position-- {
		if !strings.Contains(string(envelopes[position]), `"listRef":`) {
			t.Fatalf("item %d = %s, want a list reference", position, envelopes[position])
		}
		item, empty, err := codec.DecodeItem(context.Background(), envelopes[position])
		if err != nil || empty || item.Index != position || string(item.Body) != values[position] {
			t.Fatalf("item %d = %#v, %v, %v", position, item, empty, err)
		}
		// A real item pod's runner commits its output before the envelope.
		if _, err := store.Put(context.Background(), datastore.BlobKeyForBytes(item.Body), item.Body, datastore.PutOptions{ContentType: valueparam.ContentType}); err != nil {
			t.Fatal(err)
		}
		result, err := valueparam.EncodePublishedItemResult(item.Index, item.Body)
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, result)
	}
	aggregate, _ := json.Marshal(results)
	collected, err := codec.CollectResults(context.Background(), aggregate)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(collected, parameter) {
		t.Fatalf("collected = %.80s, want the original list reference", collected)
	}

	empty, err := codec.ExpandItems(context.Background(), []byte(`[]`))
	if err != nil || string(empty) != `[{"empty":true}]` {
		t.Fatalf("empty Argo items = %s, %v", empty, err)
	}
	if collected, err := codec.CollectResults(context.Background(), []byte(`[{"empty":true}]`)); err != nil || string(collected) != `[]` {
		t.Fatalf("empty collection = %s, %v", collected, err)
	}
}

func TestExpandRejectsMapsWiderThanArgoCanCollect(t *testing.T) {
	codec, _ := newCodec(t)
	items := strings.TrimSuffix(strings.Repeat("0,", valueparam.MaxMapItems+1), ",")
	parameter, err := codec.Encode(context.Background(), []byte("["+items+"]"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.ExpandItems(context.Background(), parameter); !errors.Is(err, valueparam.ErrContract) || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("wide map error = %v", err)
	}
	items = strings.TrimSuffix(strings.Repeat("0,", valueparam.MaxMapItems), ",")
	parameter, err = codec.Encode(context.Background(), []byte("["+items+"]"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.ExpandItems(context.Background(), parameter); err != nil {
		t.Fatalf("map at the width limit: %v", err)
	}
}

func TestCollectAcceptsArgoRawAndStringAggregates(t *testing.T) {
	codec, _ := newCodec(t)
	for name, body := range map[string][]byte{
		"raw objects":  []byte(`[{"index":1,"value":"b"},{"index":0,"value":"a"}]`),
		"JSON strings": []byte(`["{\"index\":1,\"value\":\"b\"}","{\"index\":0,\"value\":\"a\"}"]`),
	} {
		t.Run(name, func(t *testing.T) {
			collected, err := codec.CollectResults(context.Background(), body)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := string(collected), `["a","b"]`; got != want {
				t.Fatalf("collected = %s, want %s", got, want)
			}
		})
	}
}

func TestEnvelopeValidationRejectsAmbiguousOrInvalidEnvelopes(t *testing.T) {
	codec, _ := newCodec(t)
	ref := `{"hash":"sha256:` + strings.Repeat("0", 64) + `","size":5000}`
	for name, body := range map[string]string{
		"negative index":     `{"index":-1,"value":1}`,
		"extra item field":   `{"extra":true,"index":0,"value":1}`,
		"value and listRef":  `{"index":0,"listRef":` + ref + `,"value":1}`,
		"result ref as item": `{"index":0,"ref":` + ref + `}`,
		"neither value":      `{"index":0}`,
		"empty with an item": `{"empty":true,"index":0}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := codec.DecodeItem(context.Background(), []byte(body)); !errors.Is(err, valueparam.ErrContract) {
				t.Fatalf("item error = %v, want a contract violation", err)
			}
		})
	}
	small := `{"hash":"sha256:` + strings.Repeat("0", 64) + `","size":50}`
	for name, body := range map[string]string{
		"empty mixed with result":   `[{"empty":true},{"index":0,"value":1}]`,
		"missing dense result":      `[{"index":1,"value":1}]`,
		"oversized inline result":   `[{"index":0,"value":"` + strings.Repeat("v", valueparam.ItemInlineLimit) + `"}]`,
		"reference to small result": `[{"index":0,"ref":` + small + `}]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := codec.CollectResults(context.Background(), []byte(body)); err == nil {
				t.Fatal("expected collection validation failure")
			}
		})
	}
}
