#!/usr/bin/env python3
"""Rebuild internal/modelcheck/runtime_model_registries.json.

The snapshot records which model architectures the pinned vLLM and SGLang
releases register, read from their source at an exact release commit. It never
imports or runs either project: vLLM's registry tables are read with `ast`, and
each SGLang model file's `EntryClass` assignment is read the same way.

Usage (from the repository root, network and Python 3.10 or newer required):

    python3.12 -I scripts/snapshot-runtime-model-registries.py \
        --vllm-tag v0.22.1 --sglang-tag v0.5.12 \
        --output internal/modelcheck/runtime_model_registries.json

Bump the tags together with support.DefaultRuntimeVersion and
support.SGLangRuntimeVersion; a Go test fails while they disagree.
"""

import argparse
import ast
import datetime
import json
import sys
import urllib.request

VLLM_REPO = "vllm-project/vllm"
SGLANG_REPO = "sgl-project/sglang"
VLLM_REGISTRY = "vllm/model_executor/models/registry.py"
VLLM_QUANTIZATION = "vllm/model_executor/layers/quantization/__init__.py"
SGLANG_MODELS = "python/sglang/srt/models"

# vLLM registry tables and the task each one serves.
VLLM_TABLES = {
    "_TEXT_GENERATION_MODELS": "text_generation",
    "_MULTIMODAL_MODELS": "multimodal",
    "_EMBEDDING_MODELS": "embedding",
    "_LATE_INTERACTION_MODELS": "late_interaction",
    "_REWARD_MODELS": "reward",
    "_TOKEN_CLASSIFICATION_MODELS": "token_classification",
    "_SEQUENCE_CLASSIFICATION_MODELS": "sequence_classification",
    "_SPECULATIVE_DECODING_MODELS": "speculative_decoding",
    "_TRANSFORMERS_SUPPORTED_MODELS": "transformers_backend",
}


def fetch(url):
    request = urllib.request.Request(url, headers={"User-Agent": "infercrane-registry-snapshot"})
    with urllib.request.urlopen(request, timeout=60) as response:
        return response.read()


def fetch_json(url):
    return json.loads(fetch(url))


def tag_commit(repo, tag):
    ref = fetch_json(f"https://api.github.com/repos/{repo}/git/ref/tags/{tag}")["object"]
    while ref["type"] == "tag":
        ref = fetch_json(ref["url"])["object"]
    return ref["sha"]


def literal_dict(node):
    if not isinstance(node, ast.Dict):
        return {}
    out = {}
    for key, value in zip(node.keys, node.values):
        if isinstance(key, ast.Constant) and isinstance(key.value, str):
            out[key.value] = ast.literal_eval(value) if isinstance(value, ast.Constant) else None
    return out


def quantization_methods(source):
    """The string literals of vLLM's `QuantizationMethods = Literal[...]`."""
    for node in ast.parse(source).body:
        if isinstance(node, ast.Assign) and any(isinstance(target, ast.Name) and target.id == "QuantizationMethods" for target in node.targets):
            if isinstance(node.value, ast.Subscript):
                values = node.value.slice.elts if isinstance(node.value.slice, ast.Tuple) else [node.value.slice]
                return sorted(value.value for value in values if isinstance(value, ast.Constant) and isinstance(value.value, str))
    sys.exit("vLLM QuantizationMethods literal not found")


def vllm_snapshot(tag):
    commit = tag_commit(VLLM_REPO, tag)
    source = fetch(f"https://raw.githubusercontent.com/{VLLM_REPO}/{commit}/{VLLM_REGISTRY}").decode()
    tree = ast.parse(source)
    tables, previously, out_of_tree = {}, {}, {}
    for node in tree.body:
        if not isinstance(node, ast.Assign) or len(node.targets) != 1 or not isinstance(node.targets[0], ast.Name):
            continue
        name = node.targets[0].id
        if name in VLLM_TABLES:
            tables[VLLM_TABLES[name]] = sorted(literal_dict(node.value))
        elif name == "_PREVIOUSLY_SUPPORTED_MODELS":
            previously = literal_dict(node.value)
        elif name == "_OOT_SUPPORTED_MODELS":
            out_of_tree = literal_dict(node.value)
    missing = sorted(set(VLLM_TABLES.values()) - set(tables))
    if missing:
        sys.exit(f"vLLM {tag}: registry tables not found: {missing}")
    quantization = quantization_methods(fetch(f"https://raw.githubusercontent.com/{VLLM_REPO}/{commit}/{VLLM_QUANTIZATION}").decode())
    architectures = {}
    for table, names in tables.items():
        for name in names:
            architectures.setdefault(name, []).append(table)
    return {
        "runtime": "vllm",
        "version": tag.removeprefix("v"),
        "tag": tag,
        "commit": commit,
        "source_url": f"https://github.com/{VLLM_REPO}/blob/{commit}/{VLLM_REGISTRY}",
        "architectures": {name: sorted(tasks) for name, tasks in sorted(architectures.items())},
        "quantization_source_url": f"https://github.com/{VLLM_REPO}/blob/{commit}/{VLLM_QUANTIZATION}",
        "quantization_methods": quantization,
        "previously_supported": dict(sorted(previously.items())),
        "out_of_tree": dict(sorted(out_of_tree.items())),
    }


def entry_classes(source):
    tree = ast.parse(source)
    names = []
    for node in tree.body:
        if not isinstance(node, ast.Assign) or not any(isinstance(target, ast.Name) and target.id == "EntryClass" for target in node.targets):
            continue
        values = node.value.elts if isinstance(node.value, (ast.List, ast.Tuple)) else [node.value]
        for value in values:
            if isinstance(value, ast.Name):
                names.append(value.id)
            elif isinstance(value, ast.Attribute):
                names.append(value.attr)
    return names


def sglang_snapshot(tag):
    commit = tag_commit(SGLANG_REPO, tag)
    listing = fetch_json(f"https://api.github.com/repos/{SGLANG_REPO}/contents/{SGLANG_MODELS}?ref={commit}")
    architectures = {}
    for item in sorted(listing, key=lambda entry: entry["name"]):
        if item["type"] != "file" or not item["name"].endswith(".py") or item["name"] == "__init__.py":
            continue
        source = fetch(f"https://raw.githubusercontent.com/{SGLANG_REPO}/{commit}/{SGLANG_MODELS}/{item['name']}").decode("utf-8-sig")
        for name in entry_classes(source):
            architectures.setdefault(name, []).append(item["name"].removesuffix(".py"))
    if not architectures:
        sys.exit(f"SGLang {tag}: no EntryClass assignments found")
    return {
        "runtime": "sglang",
        "version": tag.removeprefix("v"),
        "tag": tag,
        "commit": commit,
        "source_url": f"https://github.com/{SGLANG_REPO}/tree/{commit}/{SGLANG_MODELS}",
        "architectures": {name: ["registered"] for name in sorted(architectures)},
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--vllm-tag", required=True)
    parser.add_argument("--sglang-tag", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    document = {
        "schema_version": "infercrane.runtime-model-registries/v1",
        "retrieved_at": datetime.date.today().isoformat(),
        "runtimes": [vllm_snapshot(args.vllm_tag), sglang_snapshot(args.sglang_tag)],
    }
    with open(args.output, "w", encoding="utf-8") as handle:
        json.dump(document, handle, indent=1, sort_keys=False)
        handle.write("\n")


if __name__ == "__main__":
    main()
