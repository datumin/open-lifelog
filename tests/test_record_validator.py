from tests._record_validator import validate_record

VALID = {
    "id": "0192f3a0-7b1e-7c3d-8e4f-0a1b2c3d4e5f",
    "type": "weight",
    "olf_version": "1.0",
    "occurred_at": "2026-05-28T07:05:00+09:00",
    "recorded_at": "2026-05-28T07:06:12+09:00",
    "tz": "Asia/Tokyo",
    "source": "mobile-app",
    "payload": {"weight_kg": 70.5},
}


def test_valid_record_has_no_errors():
    assert validate_record(VALID) == []


def test_bad_envelope_is_caught_before_payload():
    record = {**VALID, "source": "Bad Source!"}
    assert validate_record(record)  # envelope-level error


def test_bad_payload_is_caught():
    record = {**VALID, "payload": {"weight_kg": -1}}
    assert validate_record(record)


def test_unknown_type_reports_missing_schema():
    record = {**VALID, "type": "x.com.acme.mood"}
    errors = validate_record(record)
    assert any("no schema" in e for e in errors)
