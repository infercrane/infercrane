#!/bin/sh
set -eu

infercrane target add gpu-a --url http://worker-a:8101 --runtime vllm
infercrane target add gpu-b --url http://worker-b:8102 --runtime vllm
infercrane deploy Qwen/Qwen3-8B --name qwen-prod --targets gpu-a,gpu-b
exec infercrane serve

