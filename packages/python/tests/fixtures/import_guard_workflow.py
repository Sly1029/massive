from __future__ import annotations

import sys

from massive import StepContext


def double(context: StepContext[dict[str, int]]) -> dict[str, int]:
    # This executes after descriptor validation, archive loading and input hydration.
    # A task module that needs only context must not load authoring or S3 clients.
    for name in (
        "boto3",
        "botocore",
        "massive.builder",
        "massive.frontend",
        "massive.hashing",
        "massive._source_config",
        "asyncio",
    ):
        assert not any(item == name or item.startswith(name + ".") for item in sys.modules), name
    return {"value": context.inputs["value"] * 2}
