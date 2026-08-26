terraform {
  required_providers {
    infercrane = {
      source  = "infercrane/infercrane"
      version = "1.0.0"
    }
  }
}

provider "infercrane" {
  endpoint = "https://infercrane.internal"
}

resource "infercrane_deployment" "qwen" {
  name         = "qwen-prod"
  model        = "Qwen/Qwen3-8B"
  runtime      = "vllm"
  cloud        = "runpod"
  compute_mode = "elastic"
  gpu          = "L40S"
  min_replicas = 1
  max_replicas = 4
}
