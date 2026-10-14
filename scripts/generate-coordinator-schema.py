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
