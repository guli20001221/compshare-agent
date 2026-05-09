# Curated FAQ Corpus

`curated_faq.jsonl` is the bundled Stage 2B starter corpus used when
`USE_KNOWLEDGE_RETRIEVAL=curated` and `COMPSHARE_KNOWLEDGE_CORPUS` is unset.

This first slice intentionally covers a small customer-safe subset of the
legacy static FAQ prompt:

- billing behavior for stopped instances
- image type selection
- SSH and JupyterLab login entry points
- software ports and firewall exposure
- model suite / Claude Code credit usage

It is not a raw group-chat dump. Keep every chunk `acl="customer_safe"` and do
not add account-specific values, keys, tokens, IPs, or transcripts.
