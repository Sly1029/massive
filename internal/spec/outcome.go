package spec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/Sly1029/massive/internal/mapexec"
)

// mapItemOutcomeSchemaJSON is the item schema of a map that collects item
// failures. The succeeded variant's value is the map's itemOutputSchema; the
// failed variant carries the closed failure record that executors build.
var mapItemOutcomeSchemaJSON = fmt.Sprintf(`{"oneOf":[`+
	`{"type":"object","properties":{"status":{"const":"succeeded","type":"string"},"value":true},"required":["status","value"],"additionalProperties":false},`+
	`{"type":"object","properties":{"failure":{"type":"object","properties":{`+
	`"attempts":{"minimum":1,"type":"integer"},`+
	`"diagnostic":{"maxLength":%d,"type":"string"},`+
	`"kind":{"enum":["error","killed","non-retryable","timeout"],"type":"string"}`+
	`},"required":["attempts","diagnostic","kind"],"additionalProperties":false},`+
	`"status":{"const":"failed","type":"string"}},"required":["failure","status"],"additionalProperties":false}`+
	`]}`, mapexec.DiagnosticLimit)

// MapOutcomeListSchemaMatches reports whether outputSchema is an array of
// item outcomes wrapping itemOutputSchema. Frontends may place definitions in
// $defs and reference them, and may add titles, descriptions, and an OpenAPI
// discriminator: the comparison resolves local references and ignores those
// non-validating annotations, but otherwise requires the exact keywords of
// the outcome schema, so the declared list accepts exactly the values that
// executors build.
func MapOutcomeListSchemaMatches(outputSchema, itemOutputSchema json.RawMessage) bool {
	if !isUsableJSONSchema(outputSchema) || !isUsableJSONSchema(itemOutputSchema) {
		return false
	}
	output, ok := decodeSchemaTree(outputSchema)
	if !ok {
		return false
	}
	list, isObject := output.(map[string]any)
	if !isObject || list["type"] != "array" || list["items"] == nil {
		return false
	}
	expected, ok := expectedOutcomeList(itemOutputSchema)
	if !ok {
		return false
	}
	comparison := schemaComparison{left: output, right: expected, seen: map[[2]string]bool{}}
	return comparison.equal(list["items"], expected.(map[string]any)["items"], 0)
}

// expectedOutcomeList embeds the item schema as the succeeded value and
// hoists its $defs to the document root, where its local references resolve.
func expectedOutcomeList(itemOutputSchema json.RawMessage) (any, bool) {
	item, ok := decodeSchemaTree(itemOutputSchema)
	if !ok {
		return nil, false
	}
	outcome, ok := decodeSchemaTree([]byte(mapItemOutcomeSchemaJSON))
	if !ok {
		return nil, false
	}
	document := map[string]any{"type": "array", "items": outcome}
	if object, isObject := item.(map[string]any); isObject {
		if definitions, exists := object["$defs"]; exists {
			document["$defs"] = definitions
			value := make(map[string]any, len(object)-1)
			for key, child := range object {
				if key != "$defs" {
					value[key] = child
				}
			}
			item = value
		}
	}
	succeeded := outcome.(map[string]any)["oneOf"].([]any)[0].(map[string]any)
	succeeded["properties"].(map[string]any)["value"] = item
	return document, true
}

func decodeSchemaTree(document []byte) (any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	var tree any
	if err := decoder.Decode(&tree); err != nil {
		return nil, false
	}
	return tree, true
}

// annotationKeywords never change which instances a schema accepts.
var annotationKeywords = map[string]bool{
	"$comment": true, "$defs": true, "description": true, "discriminator": true, "examples": true, "title": true,
}

var (
	singleSchemaKeywords = map[string]bool{
		"$ref": true, "additionalItems": true, "additionalProperties": true, "contains": true, "else": true, "if": true,
		"items": true, "not": true, "propertyNames": true, "then": true, "unevaluatedItems": true, "unevaluatedProperties": true,
	}
	schemaListKeywords = map[string]bool{"allOf": true, "anyOf": true, "oneOf": true, "prefixItems": true}
	schemaMapKeywords  = map[string]bool{"dependentSchemas": true, "patternProperties": true, "properties": true}
)

// maxSchemaComparisonDepth stops a comparison that cannot terminate, such as
// a reference chain that alternates between unrelated definitions.
const maxSchemaComparisonDepth = 256

type schemaComparison struct {
	left, right any
	// seen holds reference pairs already assumed equal, so recursive
	// definitions compare coinductively.
	seen map[[2]string]bool
}

func (c schemaComparison) equal(left, right any, depth int) bool {
	if depth > maxSchemaComparisonDepth {
		return false
	}
	left, leftRef, ok := resolveSchemaReference(c.left, left)
	if !ok {
		return false
	}
	right, rightRef, ok := resolveSchemaReference(c.right, right)
	if !ok {
		return false
	}
	if leftRef != "" || rightRef != "" {
		pair := [2]string{leftRef, rightRef}
		if leftRef != "" && rightRef != "" && c.seen[pair] {
			return true
		}
		c.seen[pair] = true
	}
	leftObject, leftIsObject := left.(map[string]any)
	rightObject, rightIsObject := right.(map[string]any)
	if !leftIsObject || !rightIsObject {
		return reflect.DeepEqual(left, right)
	}
	leftKeys, rightKeys := validatingKeywords(leftObject), validatingKeywords(rightObject)
	if !reflect.DeepEqual(leftKeys, rightKeys) {
		return false
	}
	for keyword := range leftKeys {
		leftValue, rightValue := leftObject[keyword], rightObject[keyword]
		switch {
		case singleSchemaKeywords[keyword]:
			if keyword == "$ref" {
				leftValue, rightValue = map[string]any{"$ref": leftValue}, map[string]any{"$ref": rightValue}
			}
			if !c.equal(leftValue, rightValue, depth+1) {
				return false
			}
		case schemaListKeywords[keyword]:
			leftList, leftIsList := leftValue.([]any)
			rightList, rightIsList := rightValue.([]any)
			if !leftIsList || !rightIsList || len(leftList) != len(rightList) {
				return false
			}
			for index := range leftList {
				if !c.equal(leftList[index], rightList[index], depth+1) {
					return false
				}
			}
		case schemaMapKeywords[keyword]:
			leftMap, leftIsMap := leftValue.(map[string]any)
			rightMap, rightIsMap := rightValue.(map[string]any)
			if !leftIsMap || !rightIsMap || len(leftMap) != len(rightMap) {
				return false
			}
			for name, leftChild := range leftMap {
				rightChild, exists := rightMap[name]
				if !exists || !c.equal(leftChild, rightChild, depth+1) {
					return false
				}
			}
		default:
			if !reflect.DeepEqual(leftValue, rightValue) {
				return false
			}
		}
	}
	return true
}

func validatingKeywords(schema map[string]any) map[string]bool {
	keywords := make(map[string]bool, len(schema))
	for keyword := range schema {
		if !annotationKeywords[keyword] {
			keywords[keyword] = true
		}
	}
	return keywords
}

// resolveSchemaReference follows a local reference whose siblings are only
// annotations, returning the target and the last pointer it followed.
func resolveSchemaReference(document, schema any) (any, string, bool) {
	followed := ""
	for range maxSchemaComparisonDepth {
		object, isObject := schema.(map[string]any)
		if !isObject {
			return schema, followed, true
		}
		reference, isString := object["$ref"].(string)
		if !isString {
			return schema, followed, true
		}
		for keyword := range object {
			if keyword != "$ref" && !annotationKeywords[keyword] {
				return schema, followed, true
			}
		}
		target, ok := schemaPointer(document, reference)
		if !ok {
			return nil, "", false
		}
		schema, followed = target, reference
	}
	return nil, "", false
}

func schemaPointer(document any, reference string) (any, bool) {
	if !strings.HasPrefix(reference, "#") {
		return nil, false
	}
	fragment := reference[1:]
	if fragment == "" {
		return document, true
	}
	if !strings.HasPrefix(fragment, "/") {
		return nil, false
	}
	current := document
	for _, encoded := range strings.Split(fragment[1:], "/") {
		token, ok := decodeJSONPointerToken(encoded)
		if !ok {
			return nil, false
		}
		switch typed := current.(type) {
		case map[string]any:
			next, exists := typed[token]
			if !exists {
				return nil, false
			}
			current = next
		case []any:
			index, err := strconv.Atoi(token)
			if err != nil || !isJSONPointerArrayIndex(token) || index >= len(typed) {
				return nil, false
			}
			current = typed[index]
		default:
			return nil, false
		}
	}
	return current, true
}
