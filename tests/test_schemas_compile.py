import json
from pathlib import Path

import pytest
from jsonschema import Draft202012Validator

ROOT = Path(__file__).resolve().parents[1]
SCHEMAS = ROOT / "schemas"


@pytest.mark.parametrize(
    "schema_path",
    sorted(SCHEMAS.rglob("*.json")),
    ids=lambda p: str(p.relative_to(SCHEMAS)),
)
def test_schema_is_valid_2020_12(schema_path):
    schema = json.loads(schema_path.read_text())
    Draft202012Validator.check_schema(schema)
