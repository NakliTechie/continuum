#!/usr/bin/env python3
"""Assert schema.json actually describes the fixtures it claims to.

The published contract drifted from the validator once already: three fixtures in
testdata/valid were rejected by schema.json while both code ingresses accepted
them, and nothing noticed because no gate ever opened the file. C4's spec editor
validates against schema.json, so a schema that disagrees with the validator is a
bug the author sees, not a detail.

  valid/       must satisfy schema.json as written
  invalid/     must be refused by the CODE (not necessarily by the schema, which
               cannot express cross-field rules like undeclared ${VAR})
  normalises/  need not satisfy schema.json — JSON Schema cannot trim
"""
import json, pathlib, sys

try:
    from jsonschema import Draft202012Validator
except ImportError:
    sys.exit("schema-check: needs `pip install jsonschema` — this gate must not be skipped silently")

here = pathlib.Path(__file__).parent
validator = Draft202012Validator(json.loads((here / "schema.json").read_text()))

failures = 0
for f in sorted((here / "testdata" / "valid").glob("*.json")):
    errs = list(validator.iter_errors(json.loads(f.read_text())))
    if errs:
        failures += 1
        print(f"REJECTED {f.name}: {errs[0].message[:160]}")
print(f"schema-check: {len(list((here/'testdata'/'valid').glob('*.json')))} valid fixtures, {failures} rejected by the published schema")
sys.exit(1 if failures else 0)
