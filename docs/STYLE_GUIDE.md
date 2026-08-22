# InferCrane documentation style guide

## Information architecture

Public navigation follows a user journey:

1. **Get started** — understand the problem, evaluate fit, run one request.
2. **Build** — create or connect a stable endpoint.
3. **Operate** — keep it healthy, scalable, explainable, and safe to change.
4. **Intelligence** — benchmark, compare, recommend, and verify decisions.
5. **Integrate** — connect providers, runtimes, SDKs, and delivery tools.
6. **Reference** — exact commands, APIs, setup, runbooks, and troubleshooting.

Release notes, milestone specifications, testing ledgers, ADRs, security reviews, and qualification
procedures remain linkable repository documentation but do not belong in the primary user sidebar.
Promote one only when a user needs it to complete a product task.

Every public page needs frontmatter with a specific `title` and outcome-oriented `description`. Begin
with the result or problem, put a working example near the top, state destructive/billable boundaries
before the command, and end with the most likely next action.

Mintlify generates `/llms.txt`, `/llms-full.txt`, `/.well-known/llms.txt`, and Markdown page variants
from public navigation and frontmatter. Do not commit a custom `llms.txt` unless the generated index
cannot express a required agent directive: a custom file replaces automatic generation and must then
be maintained manually.

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
| Accent | `#60A5FA` | `#1E40AF` |
| Healthy | `#44D7B6` | `#18A98C` |
| Information | `#59A8FF` | `#297ED1` |
| Warning | `#F5B942` | `#A66C00` |
| Error | `#FF5667` | `#D6384C` |

## Typography

Use Albert Sans for headings and body, followed by Avenir Next, Helvetica Neue, Arial, and the system sans-serif fallback. Use Geist Mono, then the system monospace fallback, for commands, code, identifiers, measurements, and compact system labels. Do not commit unlicensed fonts.

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

- Mintlify code uses the repository-owned Shiki CSS-variable palette in `style.css`: blue for control
  flow, green for strings and values, violet for parameters, amber for constants, and quiet gray for
  comments. Do not introduce a second syntax-highlighting theme in individual pages.
- Product surfaces use the shared `CopyCode` component from `@infercrane/ui`. Do not recreate
  terminal chrome in page-level CSS. Bash examples show prompts visually, but copied text must never
  contain prompt characters.
- Keep code chrome quiet: one terminal glyph, one task-oriented title, a compact language label, and
  one copy action. Do not use faux macOS traffic lights or decorative window controls.
- Give the first or most important example in a section a short filename-style title so readers can
  identify it before copying. Keep titles task-oriented, such as `Deploy a model` or `Send a request`.
- Use `bash` for commands, `console` when command output is included, `json` for HTTP bodies, and `yaml` for DeploymentSpec.
- Separate commands from output unless the relationship matters.
- Every public command must exist in `cmd/infercrane/main.go` and be practical in the documented context.
- Use placeholders in uppercase (`OPERATION_ID`) and never include real secrets.
- Do not show invented timings, prices, benchmark results, success marks, or provider output.

## Terminology and claims

- Say **endpoint** for the stable application-facing identity; **deployment** for one lifecycle-managed serving realization; **replica** for one runtime worker intent; **target** for a registered existing worker.
- Say **composition** when InferCrane records identity, access, or evidence for an externally owned gateway, sandbox, or training system. Do not call composition a managed integration unless InferCrane actually owns its lifecycle.
- Say **control plane** for durable state and lifecycle; **data plane** for OpenAI-compatible request routing.
- Lead with InferCrane's provider-neutral lifecycle and adapter architecture. Name RunPod or vLLM when documenting the first qualified implementation, setup, behavior, or limitation.
- State the exact provider/runtime/mode and evidence tier rather than implying every registered
  combination is production-qualified or that InferCrane is coupled to one provider.
- Release Guard is deterministic, persisted, and evidence-bound. It is not an LLM decision.
- Context Passport persists bounded logical session identity and preferred-backend hints. It does not guarantee durable KV state or backend request migration.
- Label unqualified or incomplete capabilities as experimental. Never turn planned work into present-tense product copy.

## Maintenance

Run `npm run check` from `docs/` before review. Update navigation in `docs.json`. Reuse existing feature pages instead of duplicating explanations. When behavior changes, update the relevant page in the same change and verify it against implementation and tests.

The separately maintained InferCrane web repository owns the product console tokens. Keep semantic
roles and terminology aligned through documented design tokens; do not copy Mintlify-generated
selectors into the product UI or couple the gateway binary to frontend assets.
