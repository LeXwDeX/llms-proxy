# Change: Align proxy auth with Azure client headers

## Why
- Clients should call the proxy exactly like Azure OpenAI, only swapping the key value. Requiring a proxy-specific header breaks SDK/tool defaults.

## What Changes
- Accept Azure-style `Authorization: Bearer <access_key>` and `api-key: <access_key>` (header or query) for proxy authentication.
- Continue stripping client auth before forwarding and use configured Azure credentials upstream.

## Impact
- Affected specs: proxy-forwarding
- Affected code: auth middleware, tests, docs
