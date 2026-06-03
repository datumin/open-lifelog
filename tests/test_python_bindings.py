"""Codegen gate: prove every schema generates clean, usable pydantic models.

This is a *quality gate*, not a shipped artifact — bindings are generated into a
temp dir here and discarded. It verifies that each schema codegens without error
and that the result round-trips real data (which type-compilation alone, e.g. a
TS gate, cannot check).
"""

import importlib.util
import inspect
import subprocess
import sys
from pathlib import Path

import pytest
from pydantic import BaseModel

ROOT = Path(__file__).resolve().parents[1]
SCHEMAS = ROOT / "schemas"
TYPES = ["envelope", "weight", "steps", "meal", "sleep"]


def _generate(schema_type: str, out_dir: Path) -> Path:
    out = out_dir / f"{schema_type}.py"
    subprocess.run(
        [
            sys.executable,
            "-m",
            "datamodel_code_generator",
            "--input",
            str(SCHEMAS / schema_type / "1.json"),
            "--input-file-type",
            "jsonschema",
            "--output",
            str(out),
            "--output-model-type",
            "pydantic_v2.BaseModel",
            "--use-schema-description",
            "--use-annotated",
            "--disable-timestamp",
        ],
        check=True,
        capture_output=True,
        text=True,
    )
    return out


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


@pytest.mark.parametrize("schema_type", TYPES)
def test_schema_codegens_to_pydantic_model(schema_type, tmp_path):
    module = _load(_generate(schema_type, tmp_path))
    models = [
        obj
        for _, obj in inspect.getmembers(module, inspect.isclass)
        if issubclass(obj, BaseModel) and obj is not BaseModel
    ]
    assert models, f"{schema_type} generated no pydantic model"


def test_weight_payload_model_round_trips(tmp_path):
    module = _load(_generate("weight", tmp_path))
    model = _model_with_field(module, "weight_kg")
    instance = model.model_validate({"weight_kg": 70.5, "body_fat_percent": 18.2})
    assert float(instance.weight_kg) == 70.5
