from importlib.resources import files

from massive import StepContext


def format_value(context: StepContext[None, int]) -> str:
    prefix = files("packaged_steps").joinpath("prefix.txt").read_text(encoding="utf-8").strip()
    return f"{prefix}:{context.inputs}"
