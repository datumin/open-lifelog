import json
from pathlib import Path

from jsonschema import Draft202012Validator

ROOT = Path(__file__).resolve().parents[1]
SCHEMAS = ROOT / "schemas"


def _validator(rel_path: str) -> Draft202012Validator:
    schema = json.loads((SCHEMAS / rel_path).read_text())
    return Draft202012Validator(
        schema, format_checker=Draft202012Validator.FORMAT_CHECKER
    )


def validate_record(record: dict) -> list[str]:
    """Two-step validation: envelope, then payload by type+major.

    Returns a list of human-readable error messages; empty means valid.
    """
    envelope_errors = [
        e.message for e in _validator("envelope/1.json").iter_errors(record)
    ]
    if envelope_errors:
        return envelope_errors

    type_name = record["type"]
    major = record["olf_version"].split(".")[0]
    schema_path = SCHEMAS / type_name / f"{major}.json"
    if not schema_path.exists():
        return [f"no schema for type {type_name!r} major {major}"]

    payload_validator = _validator(f"{type_name}/{major}.json")
    return [e.message for e in payload_validator.iter_errors(record.get("payload", {}))]
