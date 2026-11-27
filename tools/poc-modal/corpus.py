#!/usr/bin/env python3
"""The prompt corpus a real-text run uses: real passages with questions, and standalone
prompts, each with its token count (docs/features/optimization.mdx, "Real text from a
prompt corpus"). The control plane composes the campaign's measured requests from the
same file (INFERCRANE_REPLAY_PROMPT_CORPUS_FILE); this module composes the traffic the
harness replays through the gateway before the campaign, and counts tokens for a new
corpus.

    corpus.py count DRAFT.json OUT.json [MODEL]
                                          fill every `tokens` with the served model's
                                          tokenizer (needs `uv`; reads tokenizer.json
                                          from Hugging Face at the pinned revision;
                                          MODEL defaults to Qwen/Qwen3.8-27B-FP8)
    corpus.py show CORPUS.json            identity, sizes and digest of a corpus
    corpus.py from-fast-track PROMPTS.json MANIFEST.json OUT.json [MODEL]
                                          the same real text the fast-track map measures
                                          with (tools/fast-track-map), as a corpus: its
                                          whole news articles as passages, its task as
                                          their question, its short prompts as standalone
                                          prompts; then counts tokens

No Modal import and no network unless `count` is run.
"""

from __future__ import annotations

import hashlib
import json
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path

SCHEMA_VERSION = "infercrane.prompt-corpus/v1"
# The corpus this repository ships for rehearsals: a dozen short passages written for it.
# Far too small for a measurement (a long request repeats passages); real runs name a
# larger open-licence corpus with --corpus.
STAND_IN = Path(__file__).resolve().parent / "testdata" / "stand-in-corpus.json"
# Prompt tokens a request spends outside its passages and question, as the control plane
# counts them (internal/replay/corpus.go).
FRAMING_TOKENS = 24


@dataclass(frozen=True)
class Corpus:
    path: Path
    digest: str
    document: dict

    @property
    def passages(self) -> list[dict]:
        return self.document["passages"]

    @property
    def prompts(self) -> list[dict]:
        return self.document["prompts"]

    @property
    def identity(self) -> dict:
        return {"digest": self.digest, "name": self.document["name"], "license": self.document["license"], "source": self.document["source"],
                "tokenizer": self.document["tokenizer"], "passages": len(self.passages), "prompts": len(self.prompts)}


def load(path: Path | str) -> Corpus:
    """Reads a corpus and checks what the control plane checks; raises ValueError."""
    path = Path(path)
    body = path.read_bytes()
    try:
        document = json.loads(body)
    except json.JSONDecodeError as error:
        raise ValueError(f"{path} is not JSON: {error}") from error
    if not isinstance(document, dict) or document.get("schema_version") != SCHEMA_VERSION:
        raise ValueError(f"{path} is not a {SCHEMA_VERSION} corpus")
    for name in ("name", "license", "source", "tokenizer"):
        if not str(document.get(name) or "").strip():
            raise ValueError(f"{path}: `{name}` must be set; a run records the licence and source of its text")
    passages, prompts = document.get("passages") or [], document.get("prompts") or []
    if not passages or not prompts:
        raise ValueError(f"{path} needs at least one passage and one standalone prompt")
    for index, passage in enumerate(passages):
        _text(passage, f"passage {index}")
        if not passage.get("questions"):
            raise ValueError(f"{path}: passage {index} has no question")
        for question in passage["questions"]:
            _text(question, f"a question of passage {index}")
    for index, prompt in enumerate(prompts):
        _text(prompt, f"prompt {index}")
    return Corpus(path, "sha256:" + hashlib.sha256(body).hexdigest(), document)


def _text(entry: dict, what: str) -> None:
    tokens = entry.get("tokens")
    if isinstance(tokens, bool) or not isinstance(tokens, int) or tokens < 1 or not str(entry.get("text") or "").strip():
        raise ValueError(f"{what} needs text and a positive token count (run `corpus.py count`)")


def compose(corpus: Corpus, hash_ids: list[int], target_tokens: int, block_tokens: int, request: int) -> tuple[list[dict], int]:
    """Messages for one replayed trace row, and the tokens planned for them.

    Each prefix block of the row holds whole passages, chosen by the block's hash alone, so
    two rows that share leading blocks send the same leading text. The system message is
    every block but the last, as in the word replay, so the gateway records the same
    shared-prefix structure; the user message is the last block's passages and a question
    about one of them. A row too short for a passage sends a standalone prompt. The system
    message opens with a line derived from the first block, so rows that share nothing do
    not share a cached prefix just because the corpus is small."""
    passages, count = corpus.passages, len(corpus.passages)
    budget = target_tokens - FRAMING_TOKENS
    blocks: list[list[int]] = []
    planned = 0
    for position, block in enumerate(hash_ids):
        limit = min((position + 1) * block_tokens, budget)
        chosen: list[int] = []
        index = (block * 2654435761) % count
        while len(chosen) < count:
            passage = passages[index % count]
            room = limit - planned - (passage["questions"][0]["tokens"] if position == len(hash_ids) - 1 else 0)
            if room < (passage["tokens"] + 1) // 2:
                break
            chosen.append(index % count)
            planned += passage["tokens"]
            index += 1
        blocks.append(chosen)
    line = f"Session {hashlib.sha256(str(hash_ids[0]).encode()).hexdigest()[:16]}"
    used = [index for block in blocks for index in block]
    if not used:
        prompt = min(corpus.prompts, key=lambda entry: (abs(entry["tokens"] - budget), entry["text"]))
        return [{"role": "system", "content": line}, {"role": "user", "content": prompt["text"]}], prompt["tokens"] + FRAMING_TOKENS
    last = blocks[-1] if blocks[-1] else []
    shared = [index for block in blocks[:-1] for index in block] if last else used[:-1]
    final = last or used[-1:]
    questions = passages[final[-1]]["questions"]
    question = questions[request % len(questions)]
    system = "\n\n".join([line, *(passages[index]["text"] for index in shared)])
    user = "\n\n".join([*(passages[index]["text"] for index in final), question["text"]])
    return [{"role": "system", "content": system}, {"role": "user", "content": user}], planned + question["tokens"] + FRAMING_TOKENS


# Counting runs in a throwaway environment so the harness itself needs no tokenizer.
COUNTER = r'''
import json, sys, urllib.request
from tokenizers import Tokenizer
model, revision, source, target = sys.argv[1:5]
with urllib.request.urlopen(urllib.request.Request(f"https://huggingface.co/{model}/resolve/{revision}/tokenizer.json", headers={"User-Agent": "infercrane-poc-modal"}), timeout=120) as response:
    tokenizer = Tokenizer.from_str(response.read().decode())
document = json.load(open(source))
def count(entry):
    entry["tokens"] = len(tokenizer.encode(entry["text"], add_special_tokens=False).ids)
for passage in document["passages"]:
    count(passage)
    for question in passage["questions"]:
        count(question)
for prompt in document["prompts"]:
    count(prompt)
document["tokenizer"] = f"{model}@{revision}"
json.dump(document, open(target, "w"), indent=2, ensure_ascii=False)
open(target, "a").write("\n")
print(json.dumps({"passages": len(document["passages"]), "prompts": len(document["prompts"]), "passage_tokens": sum(p["tokens"] for p in document["passages"])}))
'''


def count(draft: Path, target: Path, model: str, revision: str) -> None:
    completed = subprocess.run(["uv", "run", "--quiet", "--no-project", "--with", "tokenizers==0.22.1", "python", "-c", COUNTER, model, revision, str(draft), str(target)],
                               capture_output=True, text=True)
    if completed.returncode != 0:
        raise SystemExit(f"counting tokens failed: {completed.stderr.strip()[-800:]}")
    print(completed.stdout.strip())
    print(json.dumps(load(target).identity, indent=2))


FAST_TRACK_SCHEMA = "infercrane.fast-track-prompts/v1"
# Enough text that one pass of a 200-request campaign rarely repeats a passage, and small
# enough to copy into the kind node (the control plane accepts up to 64 MiB).
FAST_TRACK_PASSAGES, FAST_TRACK_PROMPTS = 1500, 1000


def from_fast_track(prompts: Path, manifest: Path, draft: Path) -> dict:
    """A corpus draft (no token counts yet) from the fast-track map's prompt set. A long
    prompt there is whole articles and a last one cut to length, joined as "Article N:"
    blocks and followed by one fixed task; only the whole articles become passages."""
    body = prompts.read_bytes()
    digest, facts = hashlib.sha256(body).hexdigest(), json.loads(manifest.read_text())
    if digest != facts["sha256"]:
        raise ValueError(f"{prompts} does not match the manifest's sha256 {facts['sha256'][:12]}…")
    document = json.loads(body)
    if document.get("schema") != FAST_TRACK_SCHEMA:
        raise ValueError(f"{prompts} is not a {FAST_TRACK_SCHEMA} prompt set")
    task = document["long_task"]

    def in_order(shape: str) -> list[dict]:
        block = document["shapes"][shape]
        return [*block["answers"], *block["check"], *(prompt for level in sorted(block["levels"], key=int) for prompt in block["levels"][level])]

    passages, seen = [], set()
    for prompt in in_order("long"):
        content = prompt["content"].removesuffix(task).rstrip()
        articles = [part.split("\n", 1)[1].strip() for part in ("\n\n" + content).split("\n\nArticle ")[1:] if "\n" in part]
        for article in articles[:-1]:  # the last one may be cut
            if article and article not in seen and len(passages) < FAST_TRACK_PASSAGES:
                seen.add(article)
                passages.append({"tokens": 0, "text": article, "questions": [{"tokens": 0, "text": task}]})
    standalone = [{"tokens": 0, "text": prompt["content"]} for prompt in in_order("short")[:FAST_TRACK_PROMPTS]]
    sources = facts["sources"]
    licences = sorted({source["licence"] for source in sources.values()})
    if len(licences) != 1:
        raise ValueError(f"the prompt set mixes licences {licences}; a corpus states one")
    corpus = {"schema_version": SCHEMA_VERSION, "name": "Fast-track prompt set v1 as a prompt corpus (oasst1 prompts, CNN/DailyMail articles)", "license": licences[0],
              "source": "; ".join(f"{source['url']} at {source['revision'][:12]} ({source['file']})" for source in (sources["long"], sources["short"]))
                        + f"; selected by tools/fast-track-map, prompt set sha256 {digest[:16]}",
              "tokenizer": "uncounted", "passages": passages, "prompts": standalone}
    draft.write_text(json.dumps(corpus, ensure_ascii=False))
    return {"passages": len(passages), "prompts": len(standalone), "prompt_set_sha256": digest}


def counted_with(arguments: list[str]):
    """The model whose tokenizer counts the corpus: the one named last, else Qwen3.8-27B-FP8."""
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    import models  # noqa: PLC0415 - only the counting commands need the pinned models

    if arguments and arguments[0] not in models.MODELS:
        raise SystemExit(f"count with one of {', '.join(models.MODELS)}, not {arguments[0]}")
    return models.MODELS[arguments[0]] if arguments else models.QWEN38_27B_FP8


def main() -> int:
    if len(sys.argv) in (5, 6) and sys.argv[1] == "from-fast-track":
        model = counted_with(sys.argv[5:])
        target = Path(sys.argv[4])
        draft = target.with_suffix(".draft.json")
        print(json.dumps(from_fast_track(Path(sys.argv[2]), Path(sys.argv[3]), draft)))
        count(draft, target, model.id, model.revision)
        draft.unlink()
        return 0
    if len(sys.argv) == 3 and sys.argv[1] == "show":
        print(json.dumps(load(sys.argv[2]).identity, indent=2))
        return 0
    if len(sys.argv) in (4, 5) and sys.argv[1] == "count":
        model = counted_with(sys.argv[4:])
        count(Path(sys.argv[2]), Path(sys.argv[3]), model.id, model.revision)
        return 0
    print(__doc__)
    return 2

if __name__ == "__main__":
    raise SystemExit(main())
