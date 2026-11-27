#!/usr/bin/env python3
"""Turns the fast-track map's task set into a task eval set the product reads.

    from_fast_track.py TASKS.json EVAL_SET.json

TASKS.json is `infercrane.fast-track-quality/v1` (tools/fast-track-map, prompts_v2.py): items
with a prompt, a kind (number or letter), a gold answer and an answer limit. The output is an
eval set with grading `answer_line` (internal/qualitycheck, task.go): the same prompts and gold
answers, ids with `:` written as `.`, and the largest answer limit of the items. Upload it with
`infercrane evaluation set upload` and name its digest as the run's `quality_eval_set`.

Standard library only. The output carries no licence of its own: the items keep the licences
the task set names (GSM8K and MMLU, MIT), which the eval set's name records.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path


def convert(document: dict) -> dict:
    if document.get("schema") != "infercrane.fast-track-quality/v1":
        raise ValueError(f"not a fast-track task set: {document.get('schema')!r}")
    prompts = []
    for item in document["items"]:
        gold = str(item["gold"]).strip()
        if item["kind"] == "letter" and gold not in ("A", "B", "C", "D"):
            raise ValueError(f"{item['id']}: a letter item's gold answer must be A to D, not {gold!r}")
        prompts.append({"id": item["id"].replace(":", "."), "prompt": item["prompt"], "expected": [gold]})
    sources = ", ".join(f"{name} ({source['licence']})" for name, source in sorted(document.get("sources", {}).items()))
    return {"schema": "infercrane.dev/quality-eval-set/v1", "name": f"fast-track-task-set-{len(prompts)}", "version": f"seed-{document.get('seed')} {sources}".strip()[:128],
            "max_tokens": max(int(item.get("max_tokens") or 1024) for item in document["items"]), "grading": "answer_line", "prompts": prompts}


def main() -> int:
    if len(sys.argv) != 3:
        print(__doc__.strip().splitlines()[2].strip(), file=sys.stderr)
        return 2
    eval_set = convert(json.loads(Path(sys.argv[1]).read_text()))
    body = json.dumps(eval_set, ensure_ascii=False, separators=(",", ":"))
    Path(sys.argv[2]).write_text(body)
    print(f"{len(eval_set['prompts'])} items, {len(body.encode()) >> 10} KiB -> {sys.argv[2]}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
