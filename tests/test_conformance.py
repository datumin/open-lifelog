import json
from pathlib import Path

import pytest
from jsonschema import Draft202012Validator

ROOT = Path(__file__).resolve().parents[1]
SCHEMAS = ROOT / "schemas"
CONFORMANCE = ROOT / "conformance"


def _validator(type_name: str) -> Draft202012Validator:
    schema = json.loads((SCHEMAS / type_name / "1.json").read_text())
    return Draft202012Validator(
        schema, format_checker=Draft202012Validator.FORMAT_CHECKER
    )


def _cases(kind: str):
    cases = []
    if not CONFORMANCE.exists():
        return cases
    for type_dir in sorted(CONFORMANCE.iterdir()):
        if not type_dir.is_dir():
            continue
        for fixture in sorted((type_dir / kind).glob("*.json")):
            cases.append(
                pytest.param(
                    type_dir.name, fixture, id=f"{type_dir.name}/{kind}/{fixture.name}"
                )
            )
    return cases


@pytest.mark.parametrize("type_name,fixture", _cases("valid"))
def test_valid_fixtures_pass(type_name, fixture):
    instance = json.loads(fixture.read_text())
    errors = [e.message for e in _validator(type_name).iter_errors(instance)]
    assert errors == [], f"expected valid, got: {errors}"


@pytest.mark.parametrize("type_name,fixture", _cases("invalid"))
def test_invalid_fixtures_fail(type_name, fixture):
    instance = json.loads(fixture.read_text())
    errors = list(_validator(type_name).iter_errors(instance))
    assert errors, "expected at least one validation error, got none"
