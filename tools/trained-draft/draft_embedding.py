"""Writes the verifier's token embedding into a speculators-format draft that has none.

speculators 0.9.0 with transformers 5.19 trains such a draft with token embeddings of zero.
`Eagle3DraftModel.from_pretrained` initialises the missing `embed_tokens` (to zero, not NaN), and
`load_verifier_weights` loads the verifier's embedding only into weights that are still NaN, so it
skips it. The draft then sees no token, only the verifier's hidden states: the trainer scored the
published AngelSlim draft at 9% first-token accuracy where vLLM, which shares the verifier's
embedding, serves it at 38%, and training under zero embeddings made the served draft worse.
Drafts converted from the original EAGLE-3 format (`speculators convert`) have no embedding;
speculators' own checkpoints carry one and need nothing.

    python draft_embedding.py DRAFT_DIR VERIFIER_DIR_OR_REPO
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

EMBEDDING = "embed_tokens.weight"
VERIFIER_EMBEDDING = "model.embed_tokens.weight"


def verifier_file(verifier: str) -> Path:
    """The verifier's safetensors file that holds its token embedding (a local directory, or a
    Hugging Face repository id as the laptop's dry run passes it)."""
    local = Path(verifier)
    if local.is_dir():
        index = local / "model.safetensors.index.json"
        return local / json.loads(index.read_text())["weight_map"][VERIFIER_EMBEDDING] if index.exists() else local / "model.safetensors"
    from huggingface_hub import hf_hub_download

    try:
        index = Path(hf_hub_download(verifier, "model.safetensors.index.json"))
        return Path(hf_hub_download(verifier, json.loads(index.read_text())["weight_map"][VERIFIER_EMBEDDING]))
    except Exception:  # noqa: BLE001 - a single-file checkpoint has no index
        return Path(hf_hub_download(verifier, "model.safetensors"))


def add_verifier_embedding(draft_dir: Path, verifier: str) -> dict:
    """Adds the verifier's embedding to draft_dir/model.safetensors when it has none, in the
    draft's dtype, and says what it did."""
    from safetensors import safe_open
    from safetensors.torch import load_file, save_file

    path = Path(draft_dir) / "model.safetensors"
    tensors = load_file(str(path))
    if EMBEDDING in tensors:
        return {"added": False, "reason": "the draft has its own embedding"}
    source = verifier_file(verifier)
    with safe_open(str(source), framework="pt") as handle:
        embedding = handle.get_tensor(VERIFIER_EMBEDDING)
    dtype = tensors["fc.weight"].dtype if "fc.weight" in tensors else embedding.dtype
    tensors[EMBEDDING] = embedding.to(dtype).contiguous()
    save_file(tensors, str(path), metadata={"format": "pt"})
    return {"added": True, "shape": list(embedding.shape), "dtype": str(dtype).removeprefix("torch."), "from": source.name}


if __name__ == "__main__":
    if len(sys.argv) != 3:
        raise SystemExit("usage: draft_embedding.py DRAFT_DIR VERIFIER_DIR_OR_REPO")
    print(json.dumps(add_verifier_embedding(Path(sys.argv[1]), sys.argv[2])))
