"""Freeze the coordinator projection from the existing shared contracts."""
import copy
import json
from pathlib import Path

root = Path(__file__).resolve().parent.parent / "conformance/schema"
workflow = json.loads((root / "workflow-spec.schema.json").read_text())
descriptor = json.loads((root / "step-invocation-descriptor.schema.json").read_text())


def obj(properties, optional=()):
    return {"type": "object", "properties": properties,
            "required": [key for key in properties if key not in optional],
            "additionalProperties": False}


def ref(name):
    return {"$ref": "#/$defs/" + name}


defs = copy.deepcopy(descriptor["$defs"])
defs["failure"] = json.loads((root / "map-item-failure.schema.json").read_text())
defs["failure"].pop("$schema", None)
defs["descriptor"] = {key: value for key, value in descriptor.items()
                      if key not in ("$defs", "$schema", "$id")}
defs["schema"] = obj({"hash": ref("hashRef"), "canonicalJson": {"type": "string"}})
defs["symbolEntry"] = obj({**defs["symbol"]["properties"], "symbolRef": ref("safePathSegment")})
contract = copy.deepcopy(workflow["$defs"]["executionContract"])
contract["properties"]["contractRef"] = ref("hashRef")
contract["required"].append("contractRef")
defs["contract"] = contract
container = copy.deepcopy(workflow["$defs"]["containerEnvironment"])
container["properties"].pop("kind")
container["required"].remove("kind")
defs["environment"] = obj({"envRef": ref("hashRef"), "container": container})
variants = []


def frame(kind, properties, optional=()):
    variants.append(obj({"kind": {"const": kind}, "protocolVersion": {"const": 1},
                         **properties}, optional))


turn = {"type": "integer", "minimum": 0, "maximum": 2147483647}
frame("CoordinatorStart", {"descriptor": ref("descriptor"), "defaults": ref("contract"),
                           "environment": ref("environment"), "deadline": {"type": "string"}},
      ("deadline",))
frame("InvokeCommand", {"turn": turn, "invocationKey": ref("safePathSegment"),
                        "symbolRef": ref("safePathSegment"), "contract": ref("contract"),
                        "input": ref("artifactRef"), "symbol": ref("symbolEntry"),
                        "schemas": {"type": "array", "items": ref("schema"), "minItems": 1},
                        "inputSchema": ref("hashRef"), "outputSchema": ref("hashRef"),
                        "environment": ref("environment")})
frame("InvokeResult", {"turn": turn, "invocationKey": ref("safePathSegment"),
                       "output": ref("artifactRef"), "failure": ref("failure")},
      ("output", "failure"))
variants[-1]["oneOf"] = [{"required": ["output"], "not": {"required": ["failure"]}},
                         {"required": ["failure"], "not": {"required": ["output"]}}]
frame("CoordinatorComplete", {"turn": turn, "output": ref("artifactRef")})
frame("CoordinatorFail", {"turn": turn, "diagnostic": {"type": "string", "maxLength": 1024},
                          "fatalMessage": {"type": "string", "minLength": 1, "maxLength": 1024},
                          "fatalType": {"type": "string", "minLength": 1, "maxLength": 64}},
      ("fatalMessage", "fatalType"))
frame("CoordinatorQuiescent", {"turn": turn})
(root / "coordinator.schema.json").write_text(json.dumps({
    "$schema": "https://json-schema.org/draft/2020-12/schema", "oneOf": variants,
    "$defs": defs}, indent=2) + "\n")

# The frontend metadata and immutable replay snapshot are separate artifacts.
# Both reuse the same shared definitions and frame projection.
spec_properties = {
    "kind": {"const": "CoordinatorSpec"}, "protocolVersion": {"const": 1},
    "symbol": ref("symbolEntry"),
    "source": obj({"packageId": ref("safePathSegment"), "language": {"const": "python"},
                   "packageHash": ref("hashRef")}),
    "defaults": ref("contract"), "environment": ref("environment"),
    "schemas": {"type": "array", "items": ref("schema"), "minItems": 2, "maxItems": 2},
    "files": {"type": "array", "minItems": 1, "items": obj({
        "path": ref("relativePath"), "hash": ref("hashRef")})},
    "inputSchema": ref("hashRef"), "outputSchema": ref("hashRef"),
    "deadlineSeconds": {"type": "integer", "minimum": 0, "maximum": 604800},
}
(root / "coordinator-spec.schema.json").write_text(json.dumps({
    "$schema": "https://json-schema.org/draft/2020-12/schema", **obj(spec_properties),
    "$defs": defs}, indent=2) + "\n")
record_defs = copy.deepcopy(defs)
record_defs["frame"] = {"oneOf": variants}
record_properties = {
    "schemaVersion": {"const": 1}, "definition": {"type": "object"},
    "start": ref("frame"), "interpreter": {"type": "string", "minLength": 1},
    "deadline": {"type": "string"},
    "turns": {"type": "array", "minItems": 1, "items": obj({
        "commands": {"type": "array", "items": ref("frame")}, "closed": {"type": "boolean"}})},
    "results": {"type": "object", "additionalProperties": ref("frame")},
    "settlements": {"type": "array", "items": {"type": "string"}},
    "deliveries": {"type": "array", "items": {"type": "string"}},
    "steps": {"type": "array", "items": {"type": "object"}},
    "retryAt": {"type": "object", "additionalProperties": {"type": "string"}},
    "sequence": {"type": "integer", "minimum": 0}, "final": ref("frame"),
}
(root / "coordinator-record.schema.json").write_text(json.dumps({
    "$schema": "https://json-schema.org/draft/2020-12/schema",
    **obj(record_properties, ("deadline", "final")), "$defs": record_defs}, indent=2) + "\n")
