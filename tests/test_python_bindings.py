import importlib.util
import inspect
import sys
from pathlib import Path

from pydantic import BaseModel

ROOT = Path(__file__).resolve().parents[1]
WEIGHT_MODULE = ROOT / "bindings" / "python" / "weight.py"


def _load(path: Path):
    spec = importlib.util.spec_from_file_location(path.stem, path)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module  # so pydantic can resolve forward refs
    spec.loader.exec_module(module)
    return module


def _model_with_field(module, field: str):
    for _, obj in inspect.getmembers(module, inspect.isclass):
        if issubclass(obj, BaseModel) and field in obj.model_fields:
            return obj
    raise AssertionError(f"no generated model has a {field!r} field")


def test_weight_payload_model_round_trips():
    module = _load(WEIGHT_MODULE)
    model = _model_with_field(module, "weight_kg")
    instance = model.model_validate({"weight_kg": 70.5, "body_fat_percent": 18.2})
    assert float(instance.weight_kg) == 70.5
