# Review Stack

This repository uses the central review stack at `madmoneymike5/review-stack-control`.

Order:

1. Native type, lint, test, and build gates.
2. OpenGrep, Gitleaks, and Trivy blocking checks.
3. Kodus local AI review, advisory.
4. CodeRabbit CLI or Codex CLI, opt-in and advisory.
5. Greptile only on request after all earlier checks pass.

Greptile is manual-only to protect the monthly credit budget. Trigger it with `@greptileai` when needed.

The weekly TruffleHog workflow is deliberately separate from normal pull-request checks.
