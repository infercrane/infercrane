# InferCrane documentation style guide

This guide keeps the public documentation precise, consistent, and maintainable. Baseten, Linear, and Vercel are quality references—not templates. Do not copy their branding, assets, copy, or component arrangements. InferCrane uses Mintlify's Maple theme for its compact documentation shell and upper-left appearance controls, with repository-owned tokens and infrastructure components layered on top.

## Visual tokens

Tokens live in `style.css`; `docs.json` contains the subset Mintlify owns. The interface is mostly neutral. Blue communicates trust, calm, and operational reliability; it identifies navigation focus and primary actions without becoming decorative. Green remains reserved for healthy runtime state.

| Role | Dark | Light |
|---|---|---|
| Background | `#0B0D0F` | `#FAF9F6` |
| Surface | `#111418` | `#FFFFFF` |
| Elevated surface | `#171B20` | `#F5F3EE` |
| Primary text | `#F3F1EA` | `#16181B` |
| Secondary text | `#9CA3AD` | `#626A73` |
| Border | `#242A31` | `#E5E2DC` |
| Accent | `#60A5FA` | `#2563EB` |
| Healthy | `#44D7B6` | `#18A98C` |
| Information | `#59A8FF` | `#297ED1` |
| Warning | `#F5B942` | `#A66C00` |
| Error | `#FF5667` | `#D6384C` |

## Typography

Use Space Grotesk for headings and body until licensed Neue Alte Grotesk files are supplied. Use Geist Mono, then the system monospace fallback, for commands, code, identifiers, measurements, and compact system labels. Do not commit unlicensed fonts.

Use regular body copy and medium or semibold headings. Keep paragraphs short. Avoid all-caps except short navigation groups, badges, and infrastructure labels.

## Spacing and components

- Prefer whitespace and thin borders to shadows.
- Use square-to-soft geometry: 6px controls, 10px system panels. Avoid pill-shaped product surfaces except status dots.
- The overview page uses `wide` mode; task and reference pages retain the default reading layout.
- Use cards to direct readers to a next action, not to turn every capability into a tile.
- Use `<Steps>` for procedures, `<Tabs>` for genuinely alternative paths, and `<Warning>` only for risk.
- Keep status displays compact: dot, label, value. Never use color as the only status signal.
- Avoid gradients, glass effects, decorative dashboards, and large rounded containers.

## Diagrams

Use Mermaid, the small `.ic-flow` system, or deterministic SVG for maintainable infrastructure diagrams. Flow from client to data plane and from CLI to control plane. Label persistence and external-provider boundaries. Do not imply a database read in the inference routing path. Animated SVGs must support light/dark mode, include `<title>` and `<desc>`, respect `prefers-reduced-motion`, and remain meaningful when animation is disabled. Avoid raster diagrams and GIFs when vector motion communicates the same idea with less weight.

## Code blocks

- Use `bash` for commands, `console` when command output is included, `json` for HTTP bodies, and `yaml` for DeploymentSpec.
- Separate commands from output unless the relationship matters.
- Every public command must exist in `cmd/infercrane/main.go` and be practical in the documented context.
- Use placeholders in uppercase (`OPERATION_ID`) and never include real secrets.
- Do not show invented timings, prices, benchmark results, success marks, or provider output.

## Terminology and claims

- Say **deployment** for the logical endpoint and desired state; **replica** for one runtime worker intent; **target** for a registered existing worker.
- Say **control plane** for durable state and lifecycle; **data plane** for OpenAI-compatible request routing.
- Lead with InferCrane's provider-neutral lifecycle and adapter architecture. Name RunPod or vLLM when documenting the first qualified implementation, setup, behavior, or limitation.
- Say **v0.1 qualifies vLLM on RunPod** rather than implying InferCrane itself is a RunPod-specific product.
- Release Guard is deterministic, persisted, and evidence-bound. It is not an LLM decision.
- Durable Session identity is deferred to v0.2 and does not guarantee durable KV state.
- Label unqualified or incomplete capabilities as experimental. Never turn planned work into present-tense product copy.

## Maintenance

Run `npm run check` from `docs/` before review. Update navigation in `docs.json`. Reuse existing feature pages instead of duplicating explanations. When behavior changes, update the relevant page in the same change and verify it against implementation and tests.

The embedded product dashboard keeps its tokens in `internal/dashboard/static/style.css`. Its role
names and semantic colors intentionally match this guide, but its assets remain release-embedded
and dependency-free. Change the two systems together when a shared visual role changes; do not copy
Mintlify-generated selectors into the product UI.
