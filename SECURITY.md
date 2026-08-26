# Security policy

Do not report suspected vulnerabilities in public issues. Use the repository's **Security** tab
and choose **Report a vulnerability** to open a private GitHub security advisory, or email
[security@infercrane.com](mailto:security@infercrane.com). Include the
affected version, impact, reproduction, and any suggested mitigation. Do not include live
credentials or customer prompt/output content.

The latest stable release receives security fixes. The main branch and release candidates are not
themselves release support commitments. Maintainers will acknowledge a private report within five
business days and coordinate disclosure after a fix is available.

Security-sensitive changes require explicit review of authentication, secret handling, external
process arguments, request/header forwarding, database permissions, migration behavior, container
contents, dependency advisories, and denial-of-service bounds.
