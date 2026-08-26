# Governance

InferCrane currently uses a maintainer-led model. Maintainers are responsible for roadmap,
security response, release integrity, architectural coherence, and respectful review.

Routine implementation decisions are made in pull requests. Durable architecture and security
decisions update the relevant public architecture, security, ownership, or integration document.
Maintainers seek evidence and consensus; when consensus is not available, the designated area owner
records the decision and tradeoffs in the pull request and resulting documentation.

The project maintainer is [@yasintoy](https://github.com/yasintoy). Path ownership is enforced by
`.github/CODEOWNERS`; additional maintainers may be added after sustained, trusted contribution.
Security reports follow [SECURITY.md](SECURITY.md), support requests follow
[SUPPORT.md](SUPPORT.md), and community participation follows
[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

Contributions use the [Developer Certificate of Origin](https://developercertificate.org/) and must
include a `Signed-off-by` line. Merging a pull request records acceptance of its contribution under
the repository's MIT License. Maintainers may decline changes that weaken product boundaries,
qualification evidence, operational safety, or long-term maintainability even when tests pass.
