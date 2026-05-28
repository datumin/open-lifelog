import importlib.util
import inspect
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
MODELS = ROOT / "bindings" / "python" / "olf_models.py"


@pytest.fixture(scope="module")
def module():
    spec = importlib.util.spec_from_file_location("olf_models", MODELS)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _model_with_field(module, field: str):
    from pydantic import BaseModel

    for _, obj in inspect.getmembers(module, inspect.isclass):
        if issubclass(obj, BaseModel) and field in obj.model_fields:
            return obj
    raise AssertionError(f"no generated model has a {field!r} field")


def test_weight_payload_model_round_trips(module):
    model = _model_with_field(module, "weight_kg")
    instance = model.model_validate({"weight_kg": 70.5, "body_fat_percent": 18.2})
    assert float(instance.weight_kg) == 70.5
