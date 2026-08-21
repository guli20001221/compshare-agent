# Prompt Change Checklist

Before changing the central Agent prompt assembled by `internal/prompt/`:

1. State the behavior being changed and keep the wording as small as possible.
2. Add or update a behavior test for routing, tool availability or a safety boundary;
   do not pin the complete prompt byte-for-byte.
3. Keep authorization and safety in code gates: tool allowlists, parameter
   validation, confirmation and destructive-action refusal.
4. Compare representative traces when the change can affect routing or token
   cost, and explain any material regression.
