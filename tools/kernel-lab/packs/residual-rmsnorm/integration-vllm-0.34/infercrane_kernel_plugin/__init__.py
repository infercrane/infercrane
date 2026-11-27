"""Serves a generated residual-add + RMSNorm kernel in vLLM 0.34 as a plugin, not a fork.

vLLM 0.34.0 removed the vLLM IR operator-priority setting the 0.22.1 integration used. It
keeps two stable hooks this module uses instead:

- `vllm.general_plugins` entry points, loaded in every engine process and limited by
  `VLLM_PLUGINS`. The `.dist-info` folder next to this package declares the entry point, so
  putting this folder on the serving container's PYTHONPATH and setting
  `VLLM_PLUGINS=infercrane_kernels` loads it; nothing is installed into the image.
- `CustomOp.register_oot`: a class registered under an in-tree op's class name replaces that
  op everywhere it is built. Its registered name becomes "RMSNorm", so a candidate enables it
  with `--compilation-config '{"custom_ops":["+RMSNorm"]}'`.

The replacement calls the generated kernel only for the shapes it was qualified on
(INFERCRANE_KERNEL_SHAPES: hidden sizes, the most rows, the dtype) and the library kernel for
every other call, the shape-keyed override with fallback FlashInfer-Bench's apply() uses. It
counts both and leaves a marker on the first generated call, so a candidate run proves the
generated code ran. Counts are taken in Python, so under CUDA graphs they count captures, not
replays. A failed install exits the process rather than quietly serving the library kernel.

Reviewed against vLLM 0.34.0's custom_op.py, layernorm.py and plugins/__init__.py. It is not
qualified on a GPU yet, so pack.json still lists vLLM 0.22.1 only.
"""

import atexit
import importlib.util
import json
import os
import sys

FAILED_INSTALL_EXIT = 97
SUPPORTED_VLLM_VERSIONS = ("0.34.0",)
_SOURCE = os.environ.get("INFERCRANE_KERNEL_SOURCE")
_STATE = os.environ.get("INFERCRANE_KERNEL_STATE")
_SHAPES = os.environ.get("INFERCRANE_KERNEL_SHAPES")
_installed = [False]


def runtime_problem(version):
    """Why this vLLM version cannot load the plugin, or None."""
    release = (version or "").split("+", 1)[0]
    if release in SUPPORTED_VLLM_VERSIONS:
        return None
    return "vLLM %s is not supported; this plugin supports vLLM %s only." % (version or "of unknown version", ", ".join(SUPPORTED_VLLM_VERSIONS))


def parse_shapes(text):
    """The qualified shapes: {"hidden": [sizes], "max_rows": n, "dtype": "bfloat16"}."""
    shapes = json.loads(text)
    hidden = shapes.get("hidden")
    if not isinstance(hidden, list) or not hidden or not all(isinstance(size, int) and 0 < size <= 65536 for size in hidden):
        raise ValueError("hidden must list the qualified hidden sizes")
    if not isinstance(shapes.get("max_rows"), int) or not 0 < shapes["max_rows"] <= 1 << 20:
        raise ValueError("max_rows must be a positive integer")
    if shapes.get("dtype") not in ("bfloat16",):
        raise ValueError("dtype must be bfloat16, the dtype the pack's contract fixes")
    return {"hidden": set(hidden), "max_rows": shapes["max_rows"], "dtype": shapes["dtype"]}


def qualifies(shapes, x, residual, weight, variance_size_override, batch_invariant):
    """Whether this call is one the generated kernel was qualified for."""
    return (
        residual is not None
        and weight is not None
        and variance_size_override is None
        and not batch_invariant
        and len(x.shape) == 2
        and x.shape[1] in shapes["hidden"]
        and x.shape[0] <= shapes["max_rows"]
        and str(x.dtype).rsplit(".", 1)[-1] == shapes["dtype"]
        and tuple(residual.shape) == tuple(x.shape)
    )


def _fail(detail):
    sys.stderr.write("infercrane kernel plugin failed: %s\n" % detail)
    sys.stderr.flush()
    os._exit(FAILED_INSTALL_EXIT)


def _load_kernel(path):
    spec = importlib.util.spec_from_file_location("infercrane_generated_kernel", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module.fused_residual_rmsnorm


def _install():
    from vllm import envs
    from vllm.model_executor.custom_op import CustomOp
    from vllm.model_executor.layers.layernorm import RMSNorm

    shapes = parse_shapes(_SHAPES)
    kernel = _load_kernel(_SOURCE)
    counts = {"generated": 0, "library": 0}
    marker = os.path.join(_STATE, "invoked")
    record = os.path.join(_STATE, "calls-%d.json" % os.getpid())

    def save():
        with open(record, "w", encoding="utf-8") as handle:
            json.dump(dict(counts, shapes={"hidden": sorted(shapes["hidden"]), "max_rows": shapes["max_rows"]}), handle)

    atexit.register(save)

    @CustomOp.register_oot(name="RMSNorm")
    class InferCraneRMSNorm(RMSNorm):
        def forward_cuda(self, x, residual=None):
            weight = self.weight.data if self.pass_weight_add else None
            if qualifies(shapes, x, residual, weight, self.variance_size_override, bool(getattr(envs, "VLLM_BATCH_INVARIANT", False))):
                if counts["generated"] == 0:
                    with open(marker, "w", encoding="utf-8"):
                        pass
                counts["generated"] += 1
                return kernel(x, residual, weight, self.variance_epsilon)
            counts["library"] += 1
            return super().forward_cuda(x, residual)

    return InferCraneRMSNorm


def register():
    """The vllm.general_plugins entry point. vLLM may call it in every process, more than
    once; it installs once per process and only in a candidate arm."""
    if _installed[0] or not (_SOURCE and _STATE and _SHAPES):
        return
    try:
        import vllm

        problem = runtime_problem(getattr(vllm, "__version__", None))
    except BaseException as error:  # noqa: BLE001 - only the type is trusted output
        _fail(type(error).__name__)
    if problem:
        _fail(problem)
    try:
        _install()
    except BaseException as error:  # noqa: BLE001 - a generated kernel's message is not trusted output
        _fail(type(error).__name__)
    _installed[0] = True
