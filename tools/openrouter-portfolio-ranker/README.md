# OpenRouter portfolio ranker

This job answers two different questions without conflating them:

1. Which open-weight models have attractive current demand relative to provider crowding?
2. For models InferCrane has profiled, which exact serving candidates can plausibly make money?

It reads OpenRouter's authenticated daily rankings, current model catalog and
live endpoint prices. Dedicated GPU cost is charged for all 730 monthly hours,
including idle capacity. Modeled or cross-provider performance is penalized and
can request a qualification campaign, but only exact hosted measurements can
produce a `launch` decision.

```bash
go run ./tools/openrouter-portfolio-ranker \
  --profiles docs/testing/evidence/openrouter-serving-profiles-2026-09-23.json \
  --lookback-days 7 \
  --market-limit 100 \
  --output /tmp/openrouter-portfolio.json
```

The default key path is `~/.config/infercrane/openrouter-key`. A report contains:

- `market_shortlist`: all inspected open-weight models, ranked by demand with a square-root provider-crowding penalty;
- `economic_candidates`: risk-adjusted throughput, blended competitive price, break-even utilization, break-even routing share, and monthly contribution scenarios;
- an explicit `launch`, `qualify`, `watch`, or `reject` decision and its evidence boundary.

This is a qualification scheduler, not an automatic purchasing or deployment
authority. Current price evidence and exact hosted qualification remain required
before InferCrane can promote a model.

The initial interactive lane excludes `:batch` variants. The FP8 weight estimate
is used only to penalize obviously expensive multi-GPU checkpoints. It does not
pretend to know sparse-model active parameters, runtime efficiency, or qualified
throughput; InferCrane's optimization campaign must measure those next.
