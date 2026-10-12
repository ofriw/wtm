# Point the catalog at a missing path: no cache, no network, so the cost is
# unknown. The Pi harness would still price itself; this repo has none.
WTM_RATES=/nonexistent/rates.json wtm status
WTM_RATES=/nonexistent/rates.json wtm --json status
