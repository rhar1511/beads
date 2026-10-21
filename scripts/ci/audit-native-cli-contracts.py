#!/usr/bin/env python3
"""Fail closed unless every explicitly selected native CLI root ran and passed."""
import hashlib
import json
import pathlib
import re
import sys

CATALOG = pathlib.Path(__file__).with_name("native-cli-contracts.txt")
PACKAGE = "github.com/rhar1511/beads/cmd/bd"


def names(path):
    selected = pathlib.Path(path).read_text().splitlines()
    if not selected or len(selected) != len(set(selected)):
        raise ValueError("empty or duplicate native contract selection")
    if any(not re.fullmatch(r"Test[A-Za-z0-9_]+", name) for name in selected):
        raise ValueError("invalid native contract name")
    return selected


def validate(path):
    selected = names(path)
    if not set(selected) <= set(names(CATALOG)):
        raise ValueError("selection contains a root outside the native contract catalog")
    return selected


def audit(selected, events):
    run = set()
    terminal = {}
    for event in events:
        if event.get("Package") != PACKAGE:
            raise ValueError("unexpected native package")
        action, test = event.get("Action"), event.get("Test", "")
        if action in {"fail", "skip"}:
            raise ValueError("native execution failed or skipped")
        if test and test.split("/", 1)[0] not in selected:
            raise ValueError("executed an unselected native root")
        if action == "run" and test in selected:
            run.add(test)
        if action == "pass":
            if test in terminal:
                raise ValueError("duplicate terminal native result")
            terminal[test] = action
    if run != set(selected) or any(terminal.get(root) != "pass" for root in selected):
        raise ValueError("a selected root did not execute and pass")
    if terminal.get("") != "pass":
        raise ValueError("native package did not pass")
    return {"package": PACKAGE, "selected_roots": selected, "all_selected_ran_and_passed": True,
            "zero_skips": True, "passed_subcases": len(terminal) - len(selected) - 1}


def main():
    mode, selection, *args = sys.argv[1:]
    selected = validate(selection)
    if mode == "validate" and not args:
        return
    if mode != "audit" or len(args) != 2:
        raise ValueError("usage: validate selected | audit selected jsonl proof")
    raw = pathlib.Path(args[0]).read_bytes()
    events = [json.loads(line) for line in raw.decode().splitlines()]
    proof = audit(selected, events)
    proof["native_json_sha256"] = hashlib.sha256(raw).hexdigest()
    proof["selection_sha256"] = hashlib.sha256(pathlib.Path(selection).read_bytes()).hexdigest()
    with pathlib.Path(args[1]).open("x") as stream:
        json.dump(proof, stream, indent=2)
        stream.write("\n")
    print(json.dumps(proof))


if __name__ == "__main__":
    main()
