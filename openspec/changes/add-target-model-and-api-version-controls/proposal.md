# Change: Enforce target model allowlists and default API versions

## Why
- Ensure each Azure endpoint only receives requests for models it actually supports, reducing accidental misroutes.
- Enforce a target-specific default `api-version` and ignore client-supplied versions to keep requests consistent with upstream deployment contracts.

## What Changes
- Add per-target configuration for allowed model deployment names; reject requests that reference models outside this allowlist.
- Add per-target configuration for a default `api-version`; proxy rewrites/sets the query parameter to this value, ignoring any client-provided value.

## Impact
- Affected specs: proxy-forwarding
- Affected code: config schema/validation, proxy request rewriting/validation, tests, config samples/docs
